// Package memory and its subpackages implement Tenant's six-tier
// memory architecture:
//
//	T0 Soul       — identity / persona / persistent user facts (internal/memory/soul)
//	T1 Working    — current conversation, sliding window (internal/memory/working — future)
//	T2 Episodic   — vector-indexed turn-pairs (internal/memory/store — future)
//	T3 Semantic   — distilled atomic facts (internal/memory/store — future)
//	T4 Procedural — named tool sequences (v1.1)
//	T5 Archive    — append-only JSONL of every event (internal/memory/archive)
//
// This file is intentionally small — the package is a namespace for the
// tier subpackages, not a fat container. See docs/MEMORY-DESIGN.md and
// docs/LOCAL-MODEL-ADAPTATION.md for the architecture.
package memory
