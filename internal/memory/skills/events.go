package skills

import "tenant/internal/logs/event"

// Skills is the skill library's event provider (docs/EVENTS.md): one event per
// change to a skill, from whichever surface made it. The Security log's audit
// records for these actions are separate (TEN-398).
var Skills = event.NewProvider("skills", "skills")

var Saved = Skills.Event(1001, event.Info, "Skill", "Skill {skill} saved ({source})", "skill", "skill_id", "source", "status", "trust", "source_runs").
	Doc("A skill was added or changed. source says how: operator, induction, auto-accept, import, seed or revert. Its earlier version is in its history (/skills history).",
		"The operator or the agent saved it, the skill-induction job proposed it, a package was imported, or a version was restored.",
		"None. /skills show names what it does; /skills diff compares it with its earlier version.")

var TrustChanged = Skills.Event(1002, event.Info, "Skill", "Skill {skill} is now {trust} (was {prior})", "skill", "skill_id", "trust", "prior", "source").
	Doc("A skill's trust changed. An untrusted skill is not offered to the agent.",
		"The operator trusted or distrusted it, or a change widened the tools it expects to use, which makes it untrusted until reviewed.",
		"If it became untrusted by a change, review it (/skills show) and trust it again if it is fine.")

var Accepted = Skills.Event(1003, event.Info, "Skill", "Skill {skill} accepted", "skill", "skill_id").
	Doc("A proposed skill went live: it is enabled and offered to the agent.",
		"The operator accepted a skill the skill-induction job proposed.",
		"None.")

var Toggled = Skills.Event(1004, event.Info, "Skill", "Skill {skill} {state}", "skill", "skill_id", "state").
	Doc("A skill was enabled or disabled. A disabled skill is not offered to the agent.",
		"The operator switched it on or off.",
		"None.")

var Tombstoned = Skills.Event(1005, event.Info, "Skill", "Skill {skill} removed", "skill", "skill_id").
	Doc("A skill was forgotten: it is disabled and hidden, and its row and history stay until a privacy forget deletes them.",
		"The operator or the agent forgot it.",
		"None. Saving a skill of the same name brings it back.")

var Imported = Skills.Event(1006, event.Info, "Package", "Skill {skill} imported from {from}", "skill", "from", "digest", "trusted", "unchanged").
	Doc("An Agent Skills package (SKILL.md) was imported as a skill. It is untrusted unless the import said otherwise.",
		"The operator ran tenant skills import or /skills import.",
		"Review it (/skills show) and trust it (/skills trust) before the agent may use it.")

var Exported = Skills.Event(1007, event.Info, "Package", "Skill {skill} exported to {dir}", "skill", "dir").
	Doc("A skill was written out as an Agent Skills package (SKILL.md).",
		"The operator ran tenant skills export or /skills export.",
		"None.")
