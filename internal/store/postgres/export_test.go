package postgres

// RecordResourcesRemovedWithHook exposes recordResourcesRemoved to the
// external e2e test package, which cannot import this package's internals but
// needs to land a delete-and-recreate between the snapshot read and the swap.
var RecordResourcesRemovedWithHook = (*Store).recordResourcesRemoved
