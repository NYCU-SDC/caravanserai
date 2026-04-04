package controller

// ConditionType mirrors api/v1.ConditionType.  It is redeclared here so that
// this package does not create a circular dependency with api/v1 before the
// store layer is wired up.  The adapter layer in cmd/cara-server converts
// between the two declarations via a simple type cast (e.g.
// controller.ConditionType(v1Cond.Type)).
type ConditionType string

const (
	ConditionTypeReady         ConditionType = "Ready"
	ConditionTypePhase         ConditionType = "Phase"
	ConditionTypeTerminatingAt ConditionType = "TerminatingAt"
	ConditionTypeNotReadyAt    ConditionType = "NotReadyAt"
)
