package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// failRunStatus makes every write moving a match run to one of statuses fail (a
// temporary trigger on the writer's one connection, as catalog's upsert tests
// inject faults), until the returned drop.
func failRunStatus(t *testing.T, e *testEnv, statuses ...string) (drop func()) {
	t.Helper()
	ctx := context.Background()
	in := "'" + strings.Join(statuses, "','") + "'"
	if _, err := e.db.ExecContext(ctx, `CREATE TEMP TRIGGER fail_run_status BEFORE UPDATE OF status ON main.match_runs
		WHEN NEW.status IN (`+in+`) BEGIN SELECT RAISE(ABORT, 'disk says no'); END`); err != nil {
		t.Fatal(err)
	}
	dropped := false
	drop = func() {
		if dropped {
			return
		}
		dropped = true
		if _, err := e.db.ExecContext(ctx, `DROP TRIGGER temp.fail_run_status`); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(drop)
	return drop
}

func runStatus(t *testing.T, e *testEnv, id int64) string {
	t.Helper()
	run, err := e.cat.GetMatchRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run.Status
}

func matchedBookURL(t *testing.T, e *testEnv, libID int64) string {
	t.Helper()
	if err := e.cat.EditBook(context.Background(), libID, unlinkFixturePath, catalog.BookEdit{
		Set:   map[string]string{catalog.FieldNarrator: "Community Reader"},
		Match: &catalog.MatchAnchor{WorkID: "the-martian"}, Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	return fixtureBookURL(libID)
}

// fixtureBookURL is the admin book URL of unlinkFixturePath in libID.
func fixtureBookURL(libID int64) string {
	return "/api/v1/admin/libraries/" + strconv.FormatInt(libID, 10) + "/book?path=" + escape(unlinkFixturePath)
}

// startRun starts a match run over libID through the API and waits for it.
func startRun(t *testing.T, e *testEnv, admin string, libID int64) int64 {
	t.Helper()
	resp, body := e.do(t, "POST", "/api/v1/admin/match-runs", admin, `{"library_id":`+strconv.FormatInt(libID, 10)+`}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d %s", resp.StatusCode, body)
	}
	e.api.matchRuns.Wait()
	return decodeRun(t, body).ID
}

// A run whose FinishMatching failed is left matching with nothing behind it:
// an Unlink isn't refused for it, Stop settles it (interrupted) and a Start
// settles any other.
func TestStuckMatchingRunIsReleased(t *testing.T) {
	t.Parallel()
	e, admin, _, libID := newMatchRunEnv(t, "us")
	stuck := func() int64 {
		drop := failRunStatus(t, e, catalog.MatchReady, catalog.MatchCancelled, catalog.MatchFailed)
		id := startRun(t, e, admin, libID)
		drop()
		if s := runStatus(t, e, id); s != catalog.MatchMatching {
			t.Fatalf("status after a failed finish = %s, want still matching", s)
		}
		return id
	}
	id := stuck()
	url := matchedBookURL(t, e, libID)
	if resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":""}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("unlink beside a stuck matching run = %d %s", resp.StatusCode, body)
	}
	stopURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(id, 10) + "/cancel"
	if resp, body := e.do(t, "POST", stopURL, admin, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("stop a stuck run = %d %s, want 204", resp.StatusCode, body)
	}
	if s := runStatus(t, e, id); s != catalog.MatchInterrupted {
		t.Fatalf("status after stop = %s, want interrupted", s)
	}
	if _, body := e.do(t, "GET", "/api/v1/admin/audit", admin, ""); !strings.Contains(body, `"orphan":true`) {
		t.Errorf("audit = %s, want the stop marked orphan", body)
	}
	if resp, body := e.do(t, "POST", stopURL, admin, ""); resp.StatusCode != http.StatusConflict || !strings.Contains(body, codeMatchRunNotRunning) {
		t.Fatalf("stop again = %d %s, want 409", resp.StatusCode, body)
	}
	// A Start settles a stuck run too.
	id = stuck()
	next := startRun(t, e, admin, libID)
	if s := runStatus(t, e, id); s != catalog.MatchInterrupted {
		t.Fatalf("stuck run after a start = %s, want interrupted", s)
	}
	if s := runStatus(t, e, next); s != catalog.MatchReady {
		t.Fatalf("next run = %s, want ready", s)
	}
}

// A run whose FinishApply failed is left applying with nothing behind it, which
// refused every Unlink of its library until a restart. An Unlink settles it and
// goes through, Stop settles it, and Apply settles and restarts it; what it
// applied stays marked.
func TestStuckApplyingRunIsReleased(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"unlink", "stop", "apply"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			e, admin, _, libID := newMatchRunEnv(t, "us")
			ctx := context.Background()
			id := startRun(t, e, admin, libID)
			runURL := "/api/v1/admin/match-runs/" + strconv.FormatInt(id, 10)
			drop := failRunStatus(t, e, catalog.MatchApplied, catalog.MatchReady)
			if resp, body := e.do(t, "POST", runURL+"/apply", admin, `{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
				t.Fatalf("apply = %d %s", resp.StatusCode, body)
			}
			e.api.matchRuns.Wait()
			drop()
			if s := runStatus(t, e, id); s != catalog.MatchApplying {
				t.Fatalf("status after a failed finish = %s, want still applying", s)
			}
			url := fixtureBookURL(libID)
			// The row alone refuses the Unlink in the catalog.
			if err := e.cat.EditBook(ctx, libID, unlinkFixturePath, catalog.BookEdit{Match: &catalog.MatchAnchor{}}); !errors.Is(err, catalog.ErrMatchRunBusy) {
				t.Fatalf("catalog unlink beside a stuck apply = %v, want ErrMatchRunBusy", err)
			}
			switch how {
			case "unlink":
				resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":""}}`)
				if resp.StatusCode != http.StatusOK || strings.Contains(body, `"community_match"`) {
					t.Fatalf("unlink beside a stuck apply = %d %s, want 200", resp.StatusCode, body)
				}
				if s := runStatus(t, e, id); s != catalog.MatchReady {
					t.Fatalf("status after the unlink = %s, want ready", s)
				}
			case "stop":
				if resp, body := e.do(t, "POST", runURL+"/cancel", admin, ""); resp.StatusCode != http.StatusNoContent {
					t.Fatalf("stop a stuck apply = %d %s, want 204", resp.StatusCode, body)
				}
				if s := runStatus(t, e, id); s != catalog.MatchReady {
					t.Fatalf("status after stop = %s, want ready", s)
				}
			case "apply":
				if resp, body := e.do(t, "POST", runURL+"/apply", admin, `{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
					t.Fatalf("apply a stuck run again = %d %s, want 202", resp.StatusCode, body)
				}
				e.api.matchRuns.Wait()
				if s := runStatus(t, e, id); s != catalog.MatchApplied {
					t.Fatalf("status after the apply = %s, want applied", s)
				}
			}
			run, err := e.cat.GetMatchRun(ctx, id)
			if err != nil || run.Counts.Applied != 1 || run.Counts.Pending != 0 {
				t.Fatalf("run = %+v %v, want its applied item still marked", run, err)
			}
			if how != "apply" {
				// Ready again, it can be applied (nothing left to apply).
				if resp, body := e.do(t, "POST", runURL+"/apply", admin, `{"scope":"fill"}`); resp.StatusCode != http.StatusAccepted {
					t.Fatalf("apply after recovery = %d %s", resp.StatusCode, body)
				}
				e.api.matchRuns.Wait()
				if s := runStatus(t, e, id); s != catalog.MatchApplied {
					t.Fatalf("status after the apply = %s, want applied", s)
				}
			}
		})
	}
}

// A settle that fails at startup (InterruptMatchRuns) leaves the runs a stopped
// server left marked working; the next Unlink or Start settles them.
func TestFailedStartupSettleRecovers(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"unlink", "start"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			e, admin, _, libID := newMatchRunEnv(t, "us")
			ctx := context.Background()
			matching, err := e.cat.StartMatchRun(ctx, catalog.MatchRun{Mode: catalog.MatchModeMatch, LibraryID: &libID})
			if err != nil {
				t.Fatal(err)
			}
			applying, err := e.cat.StartMatchRun(ctx, catalog.MatchRun{Mode: catalog.MatchModeRefresh})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.cat.FinishMatching(ctx, applying, catalog.MatchReady, ""); err != nil {
				t.Fatal(err)
			}
			if err := e.cat.BeginApply(ctx, applying, "fill", 0); err != nil {
				t.Fatal(err)
			}
			drop := failRunStatus(t, e, catalog.MatchInterrupted, catalog.MatchReady)
			if err := e.cat.InterruptMatchRuns(ctx); err == nil {
				t.Fatal("startup settle succeeded under the fault")
			}
			drop()
			if a, b := runStatus(t, e, matching), runStatus(t, e, applying); a != catalog.MatchMatching || b != catalog.MatchApplying {
				t.Fatalf("after the failed settle = %s, %s; want both still working (one transaction)", a, b)
			}
			url := matchedBookURL(t, e, libID)
			switch how {
			case "unlink":
				if resp, body := e.do(t, "PATCH", url, admin, `{"match":{"work_id":""}}`); resp.StatusCode != http.StatusOK {
					t.Fatalf("unlink = %d %s, want 200", resp.StatusCode, body)
				}
			case "start":
				if s := runStatus(t, e, startRun(t, e, admin, libID)); s != catalog.MatchReady {
					t.Fatalf("new run = %s, want ready", s)
				}
			}
			if a, b := runStatus(t, e, matching), runStatus(t, e, applying); a != catalog.MatchInterrupted || b != catalog.MatchReady {
				t.Fatalf("after the %s = %s, %s; want interrupted, ready", how, a, b)
			}
		})
	}
}
