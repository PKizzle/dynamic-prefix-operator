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

package controller

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoapplyconfigurations "k8s.io/client-go/applyconfigurations"

	"github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1/applyconfiguration"
)

// testTypeConverters is the converter chain for fake clients whose tests
// exercise Server-Side Apply on DynamicPrefix status.
//
// The fake client's default falls back to a deduced converter for CRDs, which
// treats every list as atomic -- so a test of two controllers applying their
// own condition entries would show them clobbering each other while a real API
// server, reading the CRD's listType=map, merges them. The generated converter
// carries the real schema. It only knows this operator's types, so the chain
// keeps client-go's converter for built-ins and the deduced fallback for the
// unstructured Cilium objects, which no test applies.
func testTypeConverters(scheme *runtime.Scheme) []managedfields.TypeConverter {
	return []managedfields.TypeConverter{
		applyconfiguration.NewTypeConverter(scheme),
		clientgoapplyconfigurations.NewTypeConverter(scheme),
		managedfields.NewDeducedTypeConverter(),
	}
}
