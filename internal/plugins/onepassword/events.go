package onepassword

import (
	"tenant/internal/logs/event"
	"tenant/internal/pluginreg"
)

// Events is the 1Password plugin's event catalog (TEN-393), declared by its
// descriptor. The plugin runs the op CLI rather than a web API, so it writes
// its own API-shaped events below instead of Observe's.
var Events = pluginreg.NewEvents("onepassword", "1Password")

var (
	// CLINotFound: the op binary is not where the plugin looks for it.
	CLINotFound = Events.Provider.Event(3101, event.Error, "CLI", "the 1Password CLI was not found: {err}", "err").
			Doc("The plugin could not start the op command-line tool, so no 1Password tool works.",
			"op is not installed, not on the PATH of the account Tenant runs as, or the op_path setting points at the wrong file.",
			"Install the 1Password CLI, or set op_path in `/configure onepassword` to the full path of op.")

	// AuthFailed: 1Password refused the service account token.
	AuthFailed = Events.Provider.Event(3102, event.Error, "Auth", "1Password refused the service account token: {err}", "err").
			Doc("Every call failed because 1Password did not accept the token.",
			"The token was revoked, expired, mistyped, or belongs to another account.",
			"Create a service account token for the vault the agent should use and store it with `/configure onepassword`.")

	// RateLimited: 1Password answered 429 on a call.
	RateLimited = Events.Provider.Event(2101, event.Warning, "API", "1Password rate limit reached on {op}", "op").
			Doc("A call was refused because the service account's hourly or daily request limit is used up.",
			"The agent made more calls than the plan allows (Teams and Individual: 1,000 reads and 100 writes an hour per token, 1,000 requests a day).",
			"Wait for the window to reset; cut the calls per turn; or move to a plan with a higher limit.")

	// Revealed: a secret value was read into the model's context. The
	// reference is logged; the value never is.
	Revealed = Events.Provider.Event(1101, event.Info, "Secrets", "a secret was read: {reference}", "reference").
			Doc("The agent resolved one op:// reference with op_read and received the value.",
			"The model asked for it and the egress gate allowed it.",
			"Nothing to do; this is the audit trail. Review it if a secret was read that the task did not need.")

	// ItemCreated: op_create made an item.
	ItemCreated = Events.Provider.Event(1102, event.Info, "Items", "item created: {title} in {vault}", "title", "vault").
			Doc("The agent stored a new item in a vault.", "op_create was called and the write gate allowed it.", "Nothing to do.")

	// ItemEdited: op_edit changed an item.
	ItemEdited = Events.Provider.Event(1103, event.Info, "Items", "item edited: {item}", "item").
			Doc("The agent changed fields on an item.", "op_edit was called and the write gate allowed it.", "Nothing to do.")

	// ItemArchived: op_delete archived an item.
	ItemArchived = Events.Provider.Event(1104, event.Info, "Items", "item archived: {item}", "item").
			Doc("The agent moved an item to the vault's Archive.", "op_delete was called and the destructive gate confirmed it.",
			"Restore it from the Archive in 1Password if it was a mistake.")
)
