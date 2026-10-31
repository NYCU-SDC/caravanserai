package postgres

// RecordResourcesRemovedWithHook exposes recordResourcesRemoved to the
// external e2e test package, which cannot import this package's internals but
// needs to land a delete-and-recreate between the snapshot read and the swap.
var RecordResourcesRemovedWithHook = (*Store).recordResourcesRemoved

// FinalizeProjectWithAttach exposes finalizeProject's attach hook, so the e2e
// tests can run a write inside the finalization transaction.
var FinalizeProjectWithAttach = (*Store).finalizeProject
