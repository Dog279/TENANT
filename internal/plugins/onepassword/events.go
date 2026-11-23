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

	// AuthFailed: 1Password refused the sign-in (a service account token, a
	// Connect token, or the desktop app's session).
	AuthFailed = Events.Provider.Event(3102, event.Error, "Auth", "1Password refused the sign-in: {err}", "err").
			Doc("Every call failed because 1Password did not accept the plugin's sign-in.",
			"The service account or Connect token was revoked, expired, mistyped, or belongs to another account; or, with desktop sign-in, the account is not signed in to the 1Password app.",
			"Store a current token with `/configure onepassword`, or sign in to the account in the 1Password app.")

	// AppUnavailable: desktop sign-in, and the 1Password app did not answer.
	AppUnavailable = Events.Provider.Event(3103, event.Error, "Auth", "the 1Password app did not answer the CLI: {err}", "err").
			Doc("The plugin signs in through the 1Password desktop app, and op could not reach it, so no 1Password tool works.",
			"The app is closed or locked out of reach, its `Integrate with 1Password CLI` setting is off, or Tenant runs in a session the app is not in (a service, a scheduled task, an SSH login).",
			"Open 1Password on this machine and turn on Settings, Developer, `Integrate with 1Password CLI`; run Tenant in the same desktop session; or switch the plugin to a service account or a Connect server.")

	// ConnectUnreachable: Connect sign-in, and the server did not answer.
	ConnectUnreachable = Events.Provider.Event(3104, event.Error, "API", "the 1Password Connect server did not answer: {err}", "err").
				Doc("The plugin reaches 1Password through a Connect server, and a call to it got no answer, so no 1Password tool works.",
			"The Connect containers are stopped, the address in the settings is wrong, or the network or a firewall is in the way.",
			"Start the Connect server (its API and sync containers), check `connect_host` in `/configure onepassword`, and run `tenant doctor`.")

	// NotApproved: desktop sign-in, and the app's prompt was not approved.
	NotApproved = Events.Provider.Event(2102, event.Warning, "Auth", "a 1Password request was not approved in the app: {op}", "op").
			Doc("The 1Password app asked the person at the machine to approve the agent's call, and the prompt was dismissed or timed out.",
			"Nobody answered within two minutes, or the person chose not to approve.",
			"Nothing to do if that was the intent. Otherwise ask the agent again and approve the prompt; one approval lasts until ten idle minutes pass or the app locks.")

	// RateLimited: 1Password answered 429 on a call.
	RateLimited = Events.Provider.Event(2101, event.Warning, "API", "1Password rate limit reached on {op}", "op").
			Doc("A call was refused because the service account's hourly or daily request limit is used up.",
			"The agent made more calls than the plan allows (individual, Families and Teams: 1,000 reads and 100 writes an hour per token, and 1,000 requests a day, 5,000 on Teams; Business: 10,000, 1,000 and 50,000).",
			"Wait for the window to reset; cut the calls per turn; move to a plan with a higher limit; or use a Connect server, which has no such limit.")

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
