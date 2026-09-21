// Package cloudutil is the Soul Stack SDK plumbing for plugin authors who
// drive a slow, eventually-consistent remote API: retry with backoff
// ([Retry]), wait until a resource reports ready ([WaitUntilReady]), confirm a
// deletion actually took ([ConfirmDestroy]), and classify a provider error into
// a transient/terminal taxonomy ([Classify], [FailClass]).
//
// It carries no service contract. A cloud plugin is an ordinary SoulModule
// plugin (ADR-017, ADR-020) and gets its contract from sdk/module; this package
// only holds the convergence machinery those plugins would otherwise each
// reinvent. It was extracted from the former sdk/clouddriver when the
// CloudDriver contract was removed (NIM-761), which is why the wait budget is
// still spelled [WaitBudgetEnv] = "SOUL_CLOUD_WAIT_BUDGET": the variable is set
// on deployed Keeper units, so renaming it would break a running stand.
package cloudutil
