package dashboard

// ui_adopt_test.go holds the allowlists of the page sweeps in ui_test.go
// (TEN-211). WP-ux-adopt emptied them: every template and every rendered page
// passes with no exceptions. A sweep fails on an entry that no longer matches
// anything, so a list can only grow by a deliberate, reviewed exception.

// inlineJSAllow lists templates that still carry a <script> or an on*=
// handler.
var inlineJSAllow = map[string]bool{}

// plainLanguageAllow lists, per template file (the template sweep) and per
// route (the rendered-page sweep), the banned words still on it.
var plainLanguageAllow = map[string][]string{}

// titleAllow lists the nav pages whose title isn't their nav label yet
// (DESIGN.md §2: a page's title is its nav label), by route, with the title
// each shows today.
var titleAllow = map[string]string{}
