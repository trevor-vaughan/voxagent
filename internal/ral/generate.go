package ral

import (
	"context"

	"github.com/voxagent/voxagent/internal/catalog"
)

// Generator is implemented by a provider whose resources produce further
// resources at apply time: Puppet's eval_generate (transaction.rb:111-121,
// transaction/additional_resource_generator.rb:56-128). file's recurse is the
// one implementer.
//
// The transaction calls Generate at the resource's turn in the walk, after
// every resource ordered before it and before the resource itself, so what it
// reads is the system as the run has left it, not as it was when the run
// began. It is called even when the resource is about to be skipped for a
// failed dependency, as Puppet's pre_process is.
//
// The resources it returns are applied as one unit with r: r first, then each
// child in bytewise order of Title, each through this provider's Apply. Each
// child carries GeneratedFrom = &r, its parent's Type and Tags, and no edges
// of its own: Puppet adds generated resources to the graph without their
// relationship metaparameters (event_manager.rb:65-67).
//
// managed reports whether a reference names a catalog resource; a File
// reference also matches a catalog File whose title cleans to the same path,
// as Puppet's path munge makes them one resource (type/file.rb:66-73). A
// non-nil error means nothing was generated: the transaction logs it, marks
// the report failed, and still applies r. Warnings are logged like Apply's.
//
// A Generator's type must not be able to fail Prefetch. The transaction's
// prefetch-failure branch fails and skips the resource before Generate would
// run, so its children are never generated, where Puppet would generate them
// and report each one failed too. file, the one implementer, never fails
// Prefetch.
type Generator interface {
	Generate(ctx context.Context, r catalog.Resource, managed func(ref string) bool) ([]catalog.Resource, []Warning, error)
}
