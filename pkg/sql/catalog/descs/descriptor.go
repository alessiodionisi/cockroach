// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package descs

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/lease"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// GetMutableDescriptorByID returns a mutable implementation of the descriptor
// with the requested id. An error is returned if no descriptor exists.
// Deprecated in favor of GetMutableDescriptorByIDWithFlags.
func (tc *Collection) GetMutableDescriptorByID(
	ctx context.Context, id descpb.ID, txn *kv.Txn,
) (catalog.MutableDescriptor, error) {
	return tc.GetMutableDescriptorByIDWithFlags(ctx, txn, id, tree.CommonLookupFlags{
		IncludeOffline: true,
		IncludeDropped: true,
	})
}

// GetMutableDescriptorByIDWithFlags returns a mutable implementation of the
// descriptor with the requested id. An error is returned if no descriptor exists.
// TODO (lucy): This is meant to replace GetMutableDescriptorByID. Once it does,
// rename this function.
func (tc *Collection) GetMutableDescriptorByIDWithFlags(
	ctx context.Context, txn *kv.Txn, id descpb.ID, flags tree.CommonLookupFlags,
) (catalog.MutableDescriptor, error) {
	log.VEventf(ctx, 2, "planner getting mutable descriptor for id %d", id)
	flags.RequireMutable = true
	desc, err := tc.getDescriptorByID(ctx, txn, id, flags)
	if err != nil {
		return nil, err
	}
	return desc.(catalog.MutableDescriptor), nil
}

// GetImmutableDescriptorByID returns an immmutable implementation of the
// descriptor with the requested id. An error is returned if no descriptor exists.
// Deprecated in favor of GetMutableDescriptorByIDWithFlags.
func (tc *Collection) GetImmutableDescriptorByID(
	ctx context.Context, txn *kv.Txn, id descpb.ID, flags tree.CommonLookupFlags,
) (catalog.Descriptor, error) {
	log.VEventf(ctx, 2, "planner getting immutable descriptor for id %d", id)
	flags.RequireMutable = false
	return tc.getDescriptorByID(ctx, txn, id, flags)
}

func (tc *Collection) getDescriptorByID(
	ctx context.Context, txn *kv.Txn, id descpb.ID, flags tree.CommonLookupFlags,
) (catalog.Descriptor, error) {
	return tc.getDescriptorByIDMaybeSetTxnDeadline(
		ctx, txn, id, flags, false /* setTxnDeadline */)
}

// getDescriptorByIDMaybeSetTxnDeadline returns a descriptor according to the
// provided lookup flags. Note that flags.Required is ignored, and an error is
// always returned if no descriptor with the ID exists.
func (tc *Collection) getDescriptorByIDMaybeSetTxnDeadline(
	ctx context.Context, txn *kv.Txn, id descpb.ID, flags tree.CommonLookupFlags, setTxnDeadline bool,
) (catalog.Descriptor, error) {
	getDescriptorByID := func() (catalog.Descriptor, error) {
		if vd, err := tc.maybeGetVirtualDescriptorByID(
			ctx, id, flags,
		); vd != nil || err != nil {
			return vd, err
		}

		if found, sd := tc.synthetic.getByID(id); found {
			if flags.RequireMutable {
				return nil, newMutableSyntheticDescriptorAssertionError(sd.GetID())
			}
			return sd, nil
		}
		if ud := tc.kv.getUncommittedByID(id); ud != nil {
			log.VEventf(ctx, 2, "found uncommitted descriptor %d", id)
			if flags.RequireMutable {
				return ud.mutable, nil
			}
			return ud.immutable, nil
		}

		if flags.AvoidCached || flags.RequireMutable || lease.TestingTableLeasesAreDisabled() {
			return tc.kv.getByID(ctx, txn, id, flags.RequireMutable)
		}

		// If we have already read all of the descriptor, use it as a negative
		// cache to short-circuit a lookup we know will be doomed to fail.
		//
		// TODO(ajwerner): More generally leverage this set of kv descriptors on
		// the resolution path.
		if tc.kv.idDefinitelyDoesNotExist(id) {
			return nil, catalog.ErrDescriptorNotFound
		}

		desc, shouldReadFromStore, err := tc.leased.getByID(ctx, txn, id, setTxnDeadline)
		if err != nil {
			return nil, err
		}
		if shouldReadFromStore {
			return tc.kv.getByID(ctx, txn, id, flags.RequireMutable)
		}
		return desc, nil
	}

	desc, err := getDescriptorByID()
	if err != nil {
		return nil, err
	}
	if dropped, err := filterDescriptorState(desc, true /* required */, flags); err != nil || dropped {
		// This is a special case for tables in the adding state: Roughly speaking,
		// we always need to resolve tables in the adding state by ID when they were
		// newly created in the transaction for DDL statements and for some
		// information queries (but not for ordinary name resolution for queries/
		// DML), but we also need to make these tables public in the schema change
		// job in a separate transaction.
		// TODO (lucy): We need something like an IncludeAdding flag so that callers
		// can specify this behavior, instead of having the collection infer the
		// desired behavior based on the flags (and likely producing unintended
		// behavior). See the similar comment on etDescriptorByName, which covers
		// the ordinary name resolution path as well as DDL statements.
		if desc.Adding() && (desc.IsUncommittedVersion() || flags.AvoidCached || flags.RequireMutable) {
			return desc, nil
		}
		return nil, err
	}
	return desc, nil
}

// filterDescriptorState wraps the more general catalog function to swallow
// the error if the descriptor is being dropped and the descriptor is not
// required. In that case, dropped will be true. A return value of false, nil
// means this descriptor is okay given the flags.
// TODO (lucy): We would like the ByID methods to ignore the Required flag and
// unconditionally return an error for dropped descriptors if IncludeDropped is
// not set, so we can't just pass the flags passed into the methods into this
// function, hence the boolean argument. This is the only user of
// catalog.FilterDescriptorState which needs to pass in nontrivial flags, at
// time of writing, so we should clean up the interface around this bit of
// functionality.
func filterDescriptorState(
	desc catalog.Descriptor, required bool, flags tree.CommonLookupFlags,
) (dropped bool, _ error) {
	flags = tree.CommonLookupFlags{
		Required:       required,
		IncludeOffline: flags.IncludeOffline,
		IncludeDropped: flags.IncludeDropped,
	}
	if err := catalog.FilterDescriptorState(desc, flags); err != nil {
		if required || !errors.Is(err, catalog.ErrDescriptorDropped) {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
