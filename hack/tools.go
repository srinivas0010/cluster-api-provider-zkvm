//go:build tools
// +build tools

/*
Copyright 2024 IBM Corporation.

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

// Package tools pins the exact versions of code generation binaries used
// by "make generate" and "make manifests".
// These imports are never compiled into production code — the tools build
// tag ensures they are only visible to go mod tidy.
package tools

import (
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
)
