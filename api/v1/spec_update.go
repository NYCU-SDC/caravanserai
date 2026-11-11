package v1

import "fmt"

// SpecChangeRefusal names a field a spec update may not change and why.
type SpecChangeRefusal struct {
	Field   string
	Message string
}

func (r *SpecChangeRefusal) Error() string { return r.Field + ": " + r.Message }

// CheckAssignedSpecChange reports the first change from old to next that an
// update may not make while the Project is assigned to a Node (ADR-0026):
//
//   - size: the capacity ledger books the Node by it, so a bigger size could
//     overcommit the Node the Project already runs on.
//   - removing a volume or changing its type: Managed data and its backup
//     history would no longer match a volume.
//   - backup.items: fixed at creation (CARA-126).
//
// Adding a volume is allowed. It returns nil when the update may proceed.
func CheckAssignedSpecChange(old, next ProjectSpec) *SpecChangeRefusal {
	// Compared as booked, so writing out the default size is not a change.
	if old.EffectiveSize() != next.EffectiveSize() {
		return &SpecChangeRefusal{
			Field: "spec.size",
			Message: fmt.Sprintf("cannot change from %s to %s while the Project is assigned to a Node; delete and recreate it to resize",
				old.EffectiveSize(), next.EffectiveSize()),
		}
	}
	nextTypes := make(map[string]VolumeType, len(next.Volumes))
	for _, v := range next.Volumes {
		nextTypes[v.Name] = v.Type
	}
	for _, v := range old.Volumes {
		t, kept := nextTypes[v.Name]
		if !kept {
			return &SpecChangeRefusal{
				Field:   "spec.volumes",
				Message: fmt.Sprintf("volume %q cannot be removed while the Project is assigned to a Node", v.Name),
			}
		}
		if t != v.Type {
			return &SpecChangeRefusal{
				Field:   "spec.volumes",
				Message: fmt.Sprintf("volume %q cannot change type from %s to %s", v.Name, v.Type, t),
			}
		}
	}
	if !SameBackupItems(old, next) {
		return &SpecChangeRefusal{
			Field:   "spec.backup.items",
			Message: "cannot change after the Project is created",
		}
	}
	return nil
}
