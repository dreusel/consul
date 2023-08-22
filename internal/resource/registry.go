// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package resource

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/proto-public/pbresource"
)

var (
	groupRegexp        = regexp.MustCompile(`^[a-z][a-z\d_]+$`)
	groupVersionRegexp = regexp.MustCompile(`^v([a-z\d]+)?\d$`)
	kindRegexp         = regexp.MustCompile(`^[A-Z][A-Za-z\d]+$`)
)

type Registry interface {
	// Register the given resource type and its hooks.
	Register(reg Registration)

	// Resolve the given resource type and its hooks.
	Resolve(typ *pbresource.Type) (reg Registration, ok bool)

	Types() []Registration
}

// ValidationHook is the function signature for a validation hook. These hooks can inspect
// the data as they see fit but are expected to not mutate the data in any way. If Go
// supported it, we would pass something akin to a const pointer into the callback to have
// the compiler enforce this immutability.
type ValidationHook func(*pbresource.Resource) error

// MutationHook is the function signature for a validation hook. These hooks can inspect
// and mutate the resource. If modifying the resources Data, the hook needs to ensure that
// the data gets reencoded and stored back to the Data field.
type MutationHook func(*pbresource.Resource) error

type Registration struct {
	// Type is the GVK of the resource type.
	Type *pbresource.Type

	// Proto is the resource's protobuf message type.
	Proto proto.Message

	// ACLs are hooks called to perform authorization on RPCs.
	ACLs *ACLHooks

	// Validate is called to structurally validate the resource (e.g.
	// check for required fields).
	Validate ValidationHook

	// Mutate is called to fill out any autogenerated fields (e.g. UUIDs) or
	// apply defaults before validation.
	Mutate MutationHook

	// Scope describes the tenancy scope of a resource.
	Scope Scope
}

type ACLAuthorizeReadHook func(acl.Authorizer, *acl.AuthorizerContext, *pbresource.ID) error
type ACLAuthorizeWriteHook func(acl.Authorizer, *acl.AuthorizerContext, *pbresource.Resource) error
type ACLAuthorizeListHook func(acl.Authorizer, *acl.AuthorizerContext) error

type ACLHooks struct {
	// Read is used to authorize Read RPCs and to filter results in List
	// RPCs.
	//
	// If it is omitted, `operator:read` permission is assumed.
	Read ACLAuthorizeReadHook

	// Write is used to authorize Write and Delete RPCs.
	//
	// If it is omitted, `operator:write` permission is assumed.
	Write ACLAuthorizeWriteHook

	// List is used to authorize List RPCs.
	//
	// If it is omitted, we only filter the results using Read.
	List ACLAuthorizeListHook
}

// Resource type registry
type TypeRegistry struct {
	// registrations keyed by GVK
	registrations map[string]Registration
	lock          sync.RWMutex
}

func NewRegistry() Registry {
	registry := &TypeRegistry{registrations: make(map[string]Registration)}
	// Tombstone is an implicitly registered type since it is used to implement
	// the cascading deletion of resources. ACLs end up being defaulted to
	// operator:<read,write>. It is useful to note that tombstone creation
	// does not get routed through the resource service and bypasses ACLs
	// as part of the Delete endpoint.
	registry.Register(Registration{
		Type:  TypeV1Tombstone,
		Proto: &pbresource.Tombstone{},
	})
	return registry
}

func (r *TypeRegistry) Register(registration Registration) {
	typ := registration.Type
	if typ.Group == "" || typ.GroupVersion == "" || typ.Kind == "" {
		panic("type field(s) cannot be empty")
	}

	switch {
	case !groupRegexp.MatchString(typ.Group):
		panic(fmt.Sprintf("Type.Group must be in snake_case. Got: %q", typ.Group))
	case !groupVersionRegexp.MatchString(typ.GroupVersion):
		panic(fmt.Sprintf("Type.GroupVersion must be lowercase, start with `v`, and end with a number (e.g. `v2` or `v1alpha1`). Got: %q", typ.Group))
	case !kindRegexp.MatchString(typ.Kind):
		panic(fmt.Sprintf("Type.Kind must be in PascalCase. Got: %q", typ.Kind))
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	key := ToGVK(registration.Type)
	if _, ok := r.registrations[key]; ok {
		panic(fmt.Sprintf("resource type %s already registered", key))
	}

	// set default acl hooks for those not provided
	if registration.ACLs == nil {
		registration.ACLs = &ACLHooks{}
	}
	if registration.ACLs.Read == nil {
		registration.ACLs.Read = func(authz acl.Authorizer, authzContext *acl.AuthorizerContext, id *pbresource.ID) error {
			return authz.ToAllowAuthorizer().OperatorReadAllowed(authzContext)
		}
	}
	if registration.ACLs.Write == nil {
		registration.ACLs.Write = func(authz acl.Authorizer, authzContext *acl.AuthorizerContext, id *pbresource.Resource) error {
			return authz.ToAllowAuthorizer().OperatorWriteAllowed(authzContext)
		}
	}
	if registration.ACLs.List == nil {
		registration.ACLs.List = func(authz acl.Authorizer, authzContext *acl.AuthorizerContext) error {
			return authz.ToAllowAuthorizer().OperatorReadAllowed(&acl.AuthorizerContext{})
		}
	}

	// default validation to a no-op
	if registration.Validate == nil {
		registration.Validate = func(resource *pbresource.Resource) error { return nil }
	}

	// default mutate to a no-op
	if registration.Mutate == nil {
		registration.Mutate = func(resource *pbresource.Resource) error { return nil }
	}

	r.registrations[key] = registration
}

func (r *TypeRegistry) Resolve(typ *pbresource.Type) (reg Registration, ok bool) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if registration, ok := r.registrations[ToGVK(typ)]; ok {
		return registration, true
	}
	return Registration{}, false
}

func (r *TypeRegistry) Types() []Registration {
	r.lock.RLock()
	defer r.lock.RUnlock()

	types := make([]Registration, 0, len(r.registrations))
	for _, v := range r.registrations {
		types = append(types, v)
	}
	return types
}

func ToGVK(resourceType *pbresource.Type) string {
	return fmt.Sprintf("%s.%s.%s", resourceType.Group, resourceType.GroupVersion, resourceType.Kind)
}

func ParseGVK(gvk string) (*pbresource.Type, error) {
	parts := strings.Split(gvk, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("GVK string must be in the form <Group>.<GroupVersion>.<Kind>, got: %s", gvk)
	}
	return &pbresource.Type{
		Group:        parts[0],
		GroupVersion: parts[1],
		Kind:         parts[2],
	}, nil
}
