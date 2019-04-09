// Copyright 2019 The Cloud Robotics Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package synk contains functionality to synchronize a batch of resources
// with a cluster while correctly handling CRDs and deletions.
package synk

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"time"

	apps "github.com/googlecloudrobotics/core/src/go/pkg/apis/apps/v1alpha1"
	"github.com/pkg/errors"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// Synk allows to synchronize sets of resources with a fixed cluster.
type Synk struct {
	discovery discovery.CachedDiscoveryInterface
	client    dynamic.Interface
	mapper    meta.RESTMapper
}

// New returns a new Synk object that acts against the cluster for the given configuration.
func New(client dynamic.Interface, discovery discovery.CachedDiscoveryInterface) *Synk {
	s := &Synk{
		discovery: discovery,
		client:    client,
	}
	s.mapper = restmapper.NewDeferredDiscoveryRESTMapper(discovery)
	s.mapper = restmapper.NewShortcutExpander(s.mapper, discovery)
	return s
}

// TODO: determine options that allow us to be semantically compatible with
// vanilla kubectl apply.
type ApplyOptions struct {
	name    string
	version int32
}

func (s *Synk) Apply(
	ctx context.Context,
	name string,
	opts *ApplyOptions,
	resources ...*unstructured.Unstructured,
) (*apps.ResourceSet, error) {
	if opts == nil {
		opts = &ApplyOptions{}
	}
	opts.name = name

	rs, resources, err := s.initialize(opts, resources...)
	if err != nil {
		return rs, err
	}
	err = s.applyAll(rs, opts, resources...)
	return rs, err
}

func (s *Synk) applyAll(rs *apps.ResourceSet, opts *ApplyOptions, resources ...*unstructured.Unstructured) error {
	// Initialize status for all resources.
	status := map[string]*apps.ResourceStatus{}
	for _, r := range resources {
		status[resourceKey(r)] = &apps.ResourceStatus{
			Namespace: r.GetNamespace(),
			Name:      r.GetName(),
			Action:    apps.ResourceActionNone,
		}
	}

	regulars := filter(resources, func(r *unstructured.Unstructured) bool {
		return !isCustomResourceDefinition(r)
	})
	crds := filter(resources, isCustomResourceDefinition)

	// Insert CRDs and wait for them to become available.
	for _, crd := range crds {
		st := status[resourceKey(crd)]
		// CRDs must never be replaced as deleting them will delete
		// all its current instances. Update conflicts must be resolved manually.
		action, err := s.applyOne(rs, crd, false)
		if err != nil {
			st.Error = err.Error()
		} else {
			st.Error = ""
		}
		st.Action = action
	}
	err := wait.PollImmediate(2*time.Second, 2*time.Minute, func() (bool, error) {
		for _, crd := range crds {
			if ok, err := s.crdAvailable(crd); !ok || err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		// TODO: update status.
		return errors.Wrap(err, "wait for CRDs")
	}

	// Try applying until the errors stay the same between iterations. Put in
	// an upper bound just in case of flapping errors.
	prevFailures := 0

	for i := 0; i < 10; i++ {
		curFailures := 0

		for _, r := range regulars {
			st := status[resourceKey(r)]
			// Don't retry resources that were applied successfully
			// in the first iteration.
			if i > 0 && st.Error == "" {
				continue
			}
			action, err := s.applyOne(rs, r, true)
			if err != nil {
				curFailures++
				st.Error = err.Error()
			} else {
				st.Error = ""
			}
			st.Action = action
		}
		if curFailures == 0 || curFailures == prevFailures {
			break
		}
		prevFailures = curFailures
	}
	// TODO: update status.
	// TODO: cleanup old ResourceSet versions.
	return nil
}

// initialize a new ResourceSet version for the given name and prepare resources
// for it.
func (s *Synk) initialize(
	opts *ApplyOptions,
	resources ...*unstructured.Unstructured,
) (*apps.ResourceSet, []*unstructured.Unstructured, error) {
	// Cleanup and sort resources.
	resources = filter(resources, func(r *unstructured.Unstructured) bool {
		return !reflect.DeepEqual(*r, unstructured.Unstructured{})
	})
	sortResources(resources)

	// Initialize and create next ResourceSet.
	var err error
	opts.version, err = s.next(opts.name)
	if err != nil {
		return nil, nil, errors.Wrap(err, "get next ResourceSet version")
	}

	var rs apps.ResourceSet
	rs.Name = resourceSetName(opts.name, opts.version)
	rs.Labels = map[string]string{"name": opts.name}

	groupedResources := map[schema.GroupVersionKind][]apps.ResourceRef{}
	for _, r := range resources {
		gvk := r.GroupVersionKind()
		groupedResources[gvk] = append(groupedResources[gvk], apps.ResourceRef{
			Namespace: r.GetNamespace(),
			Name:      r.GetName(),
		})
	}
	for gvk, res := range groupedResources {
		rs.Spec.Resources = append(rs.Spec.Resources, apps.ResourceSetSpecGroup{
			Group:   gvk.Group,
			Version: gvk.Version,
			Kind:    gvk.Kind,
			Items:   res,
		})
	}
	sort.Slice(rs.Spec.Resources, func(i, j int) bool {
		a, b := rs.Spec.Resources[i], rs.Spec.Resources[j]
		return fmt.Sprintf("%s/%s/%s", a.Group, a.Version, a.Kind) < fmt.Sprintf("%s/%s/%s", b.Group, b.Version, b.Kind)
	})

	rs.Status = apps.ResourceSetStatus{
		Phase:     apps.ResourceSetPhasePending,
		StartedAt: metav1.Now(),
	}
	if err := s.createResourceSet(&rs); err != nil {
		return nil, nil, errors.Wrapf(err, "create resources object %q", rs.Name)
	}
	// Attach the ResourceSet as owner. CRDs are exempt since
	// the risk of unintended deletion of all its instances is too high.
	for _, r := range resources {
		if !isCustomResourceDefinition(r) {
			if err := s.setOwnerRef(&rs, r); err != nil {
				return nil, nil, errors.Wrapf(err, "set owner reference for %q", resourceKey(r))
			}
		}
	}
	return &rs, resources, nil
}

func (s *Synk) applyOne(
	rs *apps.ResourceSet,
	resource *unstructured.Unstructured,
	replace bool,
) (apps.ResourceAction, error) {
	// If name is unset, we'd retrieve a list below and panic.
	// TODO: This may be valid if generateName is set instead. In this case we
	// want to create the resource in any case.
	if resource.GetName() == "" {
		return apps.ResourceActionNone, errors.New("missing resource name")
	}
	// GroupVersionKind is not sufficient to determine the REST API path to use
	// for the resource. We need to get this information from the RESTMapper,
	// which uses the discovery API to determine the right GroupVersionResource.
	gvk := resource.GroupVersionKind()

	mapping, err := s.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return apps.ResourceActionNone, errors.Wrap(err, "get REST mapping")
	}
	var client dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameRoot {
		client = s.client.Resource(mapping.Resource)
	} else {
		client = s.client.Resource(mapping.Resource).Namespace(resource.GetNamespace())
	}

	// Always try creating a resource first.
	prev, err := client.Get(resource.GetName(), metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		if _, err := client.Create(resource, metav1.CreateOptions{}); err != nil {
			return apps.ResourceActionCreate, errors.Wrap(err, "create resource")
		}
		return apps.ResourceActionCreate, nil
	} else if err != nil {
		return apps.ResourceActionNone, errors.Wrap(err, "get resource")
	}
	// Try to update.
	resource.SetResourceVersion(prev.GetResourceVersion())

	// TODO(freinartz): use patches.
	// TODO(freinartz): verify ownerReference conflicts here.
	if _, err = client.Update(resource, metav1.UpdateOptions{}); err == nil {
		return apps.ResourceActionUpdate, nil
	} else if !replace {
		return apps.ResourceActionUpdate, errors.Wrap(err, "update resource")
	}
	// Force update by deleting and re-creating resource. Ideally we'd only
	// do this for errors we know can be fixed by retrying. But admission validation
	// may return any status it wants, e.g. for service updates an Invalid
	// status is returned, which is also used for errors that will make create fail.
	if err := client.Delete(prev.GetName(), &metav1.DeleteOptions{}); err != nil {
		return apps.ResourceActionReplace, errors.Wrap(err, "delete resource")
	}
	resource.SetResourceVersion("")
	if _, err := client.Create(resource, metav1.CreateOptions{}); err != nil {
		return apps.ResourceActionReplace, errors.Wrap(err, "create resource")
	}
	return apps.ResourceActionReplace, nil
}

func (s *Synk) crdAvailable(ucrd *unstructured.Unstructured) (bool, error) {
	// As we are waiting for CRDs to become available, our discovery cache may still
	// have a state without it.
	s.discovery.Invalidate()

	var crd apiextensions.CustomResourceDefinition
	if err := convert(ucrd, &crd); err != nil {
		return false, err
	}
	list, err := s.discovery.ServerResourcesForGroupVersion(crd.Spec.Group + "/" + crd.Spec.Version)
	if err != nil {
		// We'd like to detect "not found" vs network errors here. But unfortunately
		// there's no canonical error being used.
		return false, nil
	}
	for _, r := range list.APIResources {
		if r.Name == crd.Spec.Names.Plural {
			return true, nil
		}
	}
	return false, nil
}

var resourceSetGVR = schema.GroupVersionResource{
	Group:    "apps.cloudrobotics.com",
	Version:  "v1alpha1",
	Resource: "resourcesets",
}

func (s *Synk) createResourceSet(rs *apps.ResourceSet) error {
	rs.Kind = "ResourceSet"
	rs.APIVersion = "apps.cloudrobotics.com/v1alpha1"

	var u unstructured.Unstructured
	if err := convert(rs, &u); err != nil {
		return err
	}
	res, err := s.client.Resource(resourceSetGVR).Create(&u, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	return convert(res, rs)
}

// next returns the next version for the resources name.
func (s *Synk) next(name string) (version int32, err error) {
	list, err := s.client.Resource(resourceSetGVR).List(metav1.ListOptions{})
	if err != nil {
		return 0, errors.Wrap(err, "list existing ResourceSets")
	}
	var curVersion int32
	for _, r := range list.Items {
		n, v, ok := decodeResourceSetName(r.GetName())
		if !ok || n != name {
			continue
		}
		if v > curVersion {
			curVersion = v
		}
	}
	return curVersion + 1, nil
}

// setOwnerRef sets the owning ResourceSet of the resource. Older ResourceSet owners
// for the same name are overwritten.
// If other conflicting ResourceSet owners are already set, an error is returned.
func (s *Synk) setOwnerRef(rs *apps.ResourceSet, resource *unstructured.Unstructured) error {
	name, version, ok := decodeResourceSetName(rs.Name)
	if !ok {
		return errors.Errorf("invalid ResourceSet name %q", rs.Name)
	}
	// An object must have at most one owner reference to a Resources object.
	var ownerRefs []metav1.OwnerReference
	for _, or := range ownerRefs {
		if or.APIVersion != "apps.cloudrobotics.com/v1alpha1" || or.Kind != "Resources" {
			ownerRefs = append(ownerRefs, or)
			continue
		}
		n, v, ok := decodeResourceSetName(or.Name)
		if !ok {
			return errors.Errorf("Resources owner reference with invalid name %q", or.Name)
		}
		if n != name {
			return errors.Errorf("owned by conflicting Resources object %q", or.Name)
		}
		if v > version {
			return errors.Errorf("conflicting Resources version %d", version)
		}
		// Valid reference to previous Resources object, skip it.
	}
	_true := true
	ownerRefs = append(ownerRefs, metav1.OwnerReference{
		APIVersion:         "apps.cloudrobotics.com/v1alpha1",
		Kind:               "Resources",
		Name:               rs.Name,
		UID:                rs.UID,
		BlockOwnerDeletion: &_true,
	})
	resource.SetOwnerReferences(ownerRefs)
	return nil
}

func isCustomResourceDefinition(r *unstructured.Unstructured) bool {
	return r.GetAPIVersion() == "apiextensions.k8s.io/v1beta1" && r.GetKind() == "CustomResourceDefinition"
}

func resourceSetName(s string, v int32) string {
	return fmt.Sprintf("%s.v%d", s, v)
}

var namePat = regexp.MustCompile(`^([a-z0-9]+)\.v([0-9]+)$`)

func decodeResourceSetName(s string) (string, int32, bool) {
	res := namePat.FindStringSubmatch(s)
	if len(res) == 0 {
		return "", 0, false
	}
	version, err := strconv.Atoi(res[2])
	if err != nil {
		panic(err)
	}
	return res[1], int32(version), true
}

func sortResources(res []*unstructured.Unstructured) {
	sort.Slice(res, func(i, j int) (b bool) {
		return resourceKey(res[i]) < resourceKey(res[j])
	})
}

func resourceKey(r *unstructured.Unstructured) string {
	gvk := r.GroupVersionKind()
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		gvk.Group,
		gvk.Version,
		gvk.Kind,
		r.GetNamespace(),
		r.GetName())
}

func filter(in []*unstructured.Unstructured, f func(*unstructured.Unstructured) bool) (out []*unstructured.Unstructured) {
	for _, r := range in {
		if f(r) {
			out = append(out, r)
		}
	}
	return out
}

// convert a resource from one type representation to another one.
func convert(from, to runtime.Object) error {
	b, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &to)
}
