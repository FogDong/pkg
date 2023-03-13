/*
Copyright 2023 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package topology

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"cuelang.org/go/cue"
	"github.com/pkg/errors"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubevela/pkg/cue/cuex"
	"github.com/kubevela/pkg/util/k8s"
	"github.com/kubevela/pkg/util/singleton"
	"github.com/kubevela/pkg/util/slices"
)

// SubResource .
type SubResource struct {
	k8s.Resource
	Children []SubResource `json:"children"`
}

// ResourceSelector .
type ResourceSelector struct {
	Group         string    `json:"group"`
	Resource      string    `json:"resource"`
	SelectorKey   string    `json:"selectorKey"`
	SelectorValue cue.Value `json:"selectorValue"`
}

type resourceTopology struct {
	ruleTemplate string
	rules        map[string]cue.Value
}

// ResourceTopology .
type ResourceTopology interface {
	GetSubResources(ctx context.Context, resource k8s.Resource) ([]SubResource, error)
	GetPeerResources(ctx context.Context, resource k8s.Resource) ([]k8s.Resource, error)
}

const (
	rulesKey         = "rules"
	subResourcesKey  = "subResources"
	peerResourcesKey = "peerResources"
	selectorKey      = "selector"

	nameSelectorKey           = "name"
	namespaceSelectorKey      = "namespace"
	builtinSelectorKey        = "builtin"
	annotationsSelectorKey    = "annotations"
	labelsSelectorKey         = "labels"
	ownerReferenceSelectorKey = "ownerReference"
)

// New .
func New(rules string) ResourceTopology {
	return &resourceTopology{
		ruleTemplate: rules,
		rules:        make(map[string]cue.Value),
	}
}

// GetSubResources get sub resources of given resource
func (r *resourceTopology) GetSubResources(ctx context.Context, resource k8s.Resource) ([]SubResource, error) {
	un, err := k8s.GetUnstructuredFromResource(ctx, singleton.KubeClient.Get(), resource)
	if err != nil {
		return nil, err
	}
	v, err := cuex.DefaultCompiler.Get().CompileStringWithOptions(ctx, r.ruleTemplate, cuex.WithExtraData("context", map[string]interface{}{
		"data": un,
	}))
	if err != nil {
		return nil, err
	}
	return r.getSubResources(ctx, v, resource)
}

func (r *resourceTopology) getSubResources(ctx context.Context, v cue.Value, resource k8s.Resource) ([]SubResource, error) {
	subResources := make([]SubResource, 0)
	rule, err := r.getRuleForResource(ctx, v, resource)
	if err != nil {
		return nil, nil
	}
	subs := rule.LookupPath(cue.ParsePath(subResourcesKey))
	if !subs.Exists() {
		return nil, nil
	}
	iter, err := subs.List()
	if err != nil {
		return nil, errors.Wrap(err, "subResources should be a list")
	}
	for iter.Next() {
		items, err := r.getResourcesWithSelector(ctx, iter.Value(), resource)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			children, err := r.getSubResources(ctx, v, item)
			if err != nil {
				return nil, err
			}
			subResources = append(subResources, SubResource{
				Resource: item,
				Children: children,
			})
		}
	}
	return subResources, nil
}

func (r *resourceTopology) getRuleForResource(ctx context.Context, v cue.Value, resource k8s.Resource) (cue.Value, error) {
	if r.rules == nil {
		r.rules = make(map[string]cue.Value)
		v = v.LookupPath(cue.ParsePath(rulesKey))
		if !v.Exists() {
			return cue.Value{}, fmt.Errorf("no rules found")
		}
		iter, err := v.List()
		if err != nil {
			return cue.Value{}, errors.Wrap(err, "rules should be a list")
		}
		for iter.Next() {
			re := &k8s.Resource{}
			if err := iter.Value().Decode(re); err != nil {
				return cue.Value{}, err
			}
			r.rules[fmt.Sprintf("%s/%s", re.Group, re.Resource)] = iter.Value()
		}
	}
	if rule, ok := r.rules[fmt.Sprintf("%s/%s", resource.Group, resource.Resource)]; ok {
		return rule, nil
	}
	return cue.Value{}, fmt.Errorf("no rule found for resource %s/%s", resource.Group, resource.Resource)
}

// GetPeerResources get peer resources of given resource
func (r *resourceTopology) GetPeerResources(ctx context.Context, resource k8s.Resource) ([]k8s.Resource, error) {
	un, err := k8s.GetUnstructuredFromResource(ctx, singleton.KubeClient.Get(), resource)
	if err != nil {
		return nil, err
	}

	v, err := cuex.DefaultCompiler.Get().CompileStringWithOptions(ctx, r.ruleTemplate, cuex.WithExtraData("context", map[string]interface{}{
		"data": un,
	}))
	if err != nil {
		return nil, err
	}
	if v.Err() != nil {
		return nil, v.Err()
	}
	rule, err := r.getRuleForResource(ctx, v, resource)
	if err != nil {
		return nil, err
	}

	return r.getPeerResources(ctx, rule, resource)
}

func (r *resourceTopology) getPeerResources(ctx context.Context, rule cue.Value, resource k8s.Resource) ([]k8s.Resource, error) {
	peer := rule.LookupPath(cue.ParsePath(peerResourcesKey))
	if !peer.Exists() {
		return nil, nil
	}
	iter, err := peer.List()
	if err != nil {
		return nil, errors.Wrap(err, "peerResources should be a list")
	}
	peerResources := make([]k8s.Resource, 0)
	for iter.Next() {
		items, err := r.getResourcesWithSelector(ctx, iter.Value(), resource)
		if err != nil {
			return nil, err
		}
		peerResources = append(peerResources, items...)
	}
	return peerResources, nil
}

func (r *resourceTopology) getResourcesWithSelector(ctx context.Context, v cue.Value, resource k8s.Resource) ([]k8s.Resource, error) {
	base := &k8s.Resource{}
	if err := v.Decode(base); err != nil {
		return nil, err
	}
	selVal := v.LookupPath(cue.ParsePath(selectorKey))
	if !selVal.Exists() {
		return nil, fmt.Errorf("selector is required")
	}
	fields, err := selVal.Fields()
	if err != nil {
		return nil, err
	}
	resources := make([]k8s.Resource, 0)
	for fields.Next() {
		switch fields.Label() {
		case builtinSelectorKey:
			typ, err := fields.Value().String()
			if err != nil {
				return nil, err
			}
			return r.handleBuiltInRules(ctx, typ, v, resource)
		case nameSelectorKey:
			nameVal := fields.Value()
			switch nameVal.Kind() {
			case cue.StringKind:
				name, err := nameVal.String()
				if err != nil {
					return nil, err
				}
				resources = append(resources, k8s.Resource{
					Group:     base.Group,
					Resource:  base.Resource,
					Name:      name,
					Namespace: resource.Namespace,
				})
			default:
				names := make([]string, 0)
				err := nameVal.Decode(&names)
				if err != nil {
					return nil, err
				}
				for _, name := range names {
					resources = append(resources, k8s.Resource{
						Group:     base.Group,
						Resource:  base.Resource,
						Name:      name,
						Namespace: resource.Namespace,
					})
				}
			}
		default:
			selector := &ResourceSelector{
				Group:         base.Group,
				Resource:      base.Resource,
				SelectorKey:   fields.Label(),
				SelectorValue: fields.Value(),
			}
			items, err := listResources(ctx, selector, resource)
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				resources = append(resources, k8s.Resource{
					Group:     selector.Group,
					Resource:  selector.Resource,
					Name:      item.GetName(),
					Namespace: item.GetNamespace(),
				})
			}
		}
	}
	return resources, nil
}

func (r *resourceTopology) handleBuiltInRules(ctx context.Context, typ string, v cue.Value, resource k8s.Resource) ([]k8s.Resource, error) {
	switch strings.ToLower(typ) {
	case "service":
		return r.handleBuiltInRulesForService(ctx, v, resource)
	default:
		return nil, fmt.Errorf("unsupported built-in rule %s", typ)
	}
}

func (r *resourceTopology) getGroupResourceFromSubs(sub SubResource, group, resource string) []k8s.Resource {
	result := make([]k8s.Resource, 0)
	if sub.Resource.Group == group && sub.Resource.Resource == resource {
		result = append(result, sub.Resource)
	}
	for _, child := range sub.Children {
		result = append(result, r.getGroupResourceFromSubs(child, group, resource)...)
	}
	return result
}

func (r *resourceTopology) handleBuiltInRulesForService(ctx context.Context, v cue.Value, resource k8s.Resource) ([]k8s.Resource, error) {
	subs, err := r.getSubResources(ctx, v, resource)
	if err != nil {
		return nil, err
	}
	pods := make([]k8s.Resource, 0)
	for _, sub := range subs {
		pods = append(pods, r.getGroupResourceFromSubs(sub, "", "Pod")...)
	}
	// get service endpoints and compare with pods
	es := &discoveryv1.EndpointSliceList{}
	if err = singleton.KubeClient.Get().List(ctx, es, client.InNamespace(resource.Namespace)); err != nil {
		return nil, err
	}
	service := []k8s.Resource{}
	for _, e := range es.Items {
		for _, s := range e.Endpoints {
			if slices.Contains(pods, k8s.Resource{
				Name:      s.TargetRef.Name,
				Namespace: s.TargetRef.Namespace,
				Group:     "",
				Resource:  s.TargetRef.Kind,
			}) {
				service = append(service, k8s.Resource{
					Group:     "",
					Resource:  "Service",
					Name:      e.OwnerReferences[0].Name,
					Namespace: resource.Namespace,
				})
			}
		}
	}
	return service, nil
}

func listResources(ctx context.Context, selector *ResourceSelector, relation k8s.Resource) ([]unstructured.Unstructured, error) {
	cli := singleton.KubeClient.Get()
	resource := k8s.Resource{
		Group:    selector.Group,
		Resource: selector.Resource,
	}
	listOpts := make([]client.ListOption, 0)
	var annos map[string]string
	var owner bool
	switch selector.SelectorKey {
	case nameSelectorKey:
		if ns, err := selector.SelectorValue.String(); err == nil {
			listOpts = append(listOpts, client.InNamespace(ns))
		}
	case labelsSelectorKey:
		labels := make(map[string]string)
		if err := selector.SelectorValue.Decode(&labels); err == nil {
			listOpts = append(listOpts, client.MatchingLabels(labels))
		}
	case annotationsSelectorKey:
		_ = selector.SelectorValue.Decode(&annos)
	case ownerReferenceSelectorKey:
		if b, err := selector.SelectorValue.Bool(); err == nil {
			owner = b
			listOpts = append(listOpts, client.InNamespace(relation.Namespace))
		}
	default:
		return nil, errors.Errorf("unknown selector [%s] for list resources", selector.SelectorKey)
	}
	gvk, err := k8s.GetGVKFromResource(ctx, cli, resource)
	if err != nil {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)
	if err := cli.List(ctx, list, listOpts...); err != nil {
		return nil, err
	}
	switch {
	case len(annos) > 0:
		filtered := make([]unstructured.Unstructured, 0)
		for _, un := range list.Items {
			if reflect.DeepEqual(un.GetAnnotations(), annos) {
				filtered = append(filtered, un)
			}
		}
		return filtered, nil
	case owner:
		filtered := make([]unstructured.Unstructured, 0)
		for _, un := range list.Items {
			for _, ref := range un.GetOwnerReferences() {
				if ref.Name == relation.Name && ref.Kind == relation.Resource {
					filtered = append(filtered, un)
				}
			}
		}
		return filtered, nil
	default:
		return list.Items, nil
	}
}
