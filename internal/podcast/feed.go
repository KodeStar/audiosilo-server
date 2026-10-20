// Package podcast implements podcast indexing helpers and the server's poll scheduler.
package podcast

import "github.com/kodestar/audiosilo-server/internal/podcast/feed"

type File = feed.File
type Show = feed.Show
type Episode = feed.Episode
type Fetch = feed.Fetch

var ReadFile = feed.ReadFile
var NormalizeFeedURL = feed.NormalizeFeedURL
var FeedURLHash = feed.FeedURLHash
var RedactURL = feed.RedactURL
var IsPrivateFeedURL = feed.IsPrivateFeedURL
