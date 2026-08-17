/*
Copyright 2026 jr42.
Copyright 2026 PKizzle.

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

// Package v1alpha1 contains API Schema definitions for the  v1alpha1 API group.
// +kubebuilder:object:generate=true
// +kubebuilder:ac:generate=true
// +groupName=dynamic-prefix.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// SchemeGroupVersion is the name the generated apply configurations expect
	// for GroupVersion; the two are the same value under the two naming
	// conventions (kubebuilder's and code-generator's).
	SchemeGroupVersion = schema.GroupVersion{Group: "dynamic-prefix.io", Version: "v1alpha1"}

	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "dynamic-prefix.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	// Deprecated in controller-runtime v0.24 on the grounds that api packages
	// should carry minimal dependencies. This is kubebuilder scaffolding and
	// still the supported way to register the group; replacing it by hand
	// would diverge from the generated layout for no functional gain.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // SA1019: see above

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
