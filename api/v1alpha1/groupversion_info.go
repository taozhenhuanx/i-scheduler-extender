/*
Copyright 2026.

*/

// +groupName=scheduler.example.com
//
// Package v1alpha1 contains API Schema definitions for the scheduler v1alpha1 API group.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "scheduler.example.com", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
