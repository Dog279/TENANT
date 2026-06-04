package distill

// Test-only hook: exposes the unexported extractJSONObject so the
// black-box _test package can verify real-model output handling
// without exporting it in the production API.
var ExtractJSONObjectForTest = extractJSONObject
