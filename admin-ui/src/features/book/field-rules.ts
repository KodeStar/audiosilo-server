// The limits catalog.normalizeOverride enforces, shared by the field checks
// (book-model.ts) and the other-series line (more-series.ts).

export const MAX_SHORT = 500;
export const MAX_SERIES_INDEX = 100000;
// unicode.IsControl: C0, DEL and C1.
// eslint-disable-next-line no-control-regex
export const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;
export const NUMBER = /^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/;
