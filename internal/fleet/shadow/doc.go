// Package shadow is the memory of a fleet's machines (TEN-532,
// docs/plans/gap6-fleet-plan.md section 6), without the link that carries it.
//
// On the master, a machine's memory is its shadow: an episodic and a semantic
// store that exist in RAM only (Store). What the master's disk holds of a
// shadow is a snapshot, encrypted to the fleet key and to a recovery key
// (Key, NewRecovery), and the newest generation made for the child. On the
// child, a machine's memory is the last three generations it was sent, which
// it cannot read (ChildStore). When the two meet again, Decide says which
// side's copy stands.
package shadow
