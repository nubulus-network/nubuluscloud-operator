/*
Copyright 2026 Nubulus Network.

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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	tunnelv1alpha1 "github.com/nubulus-network/nubuluscloud-operator/api/v1alpha1"
)

// The integration tests run against a real API server and a real etcd, started
// by envtest. There is no kubelet, so nothing ever gets scheduled: a Deployment
// created here stays at zero available replicas until a test says otherwise.
//
// That is enough for everything worth testing in a reconciler, because what a
// reconciler does is read and write API objects. It is also why no container
// runtime is needed to run any of this.
var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config
	k8sClient client.Client
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	if err := os.Setenv("KUBEBUILDER_ASSETS", assetsDir()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	restCfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		os.Exit(1)
	}

	if err := tunnelv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		fmt.Fprintf(os.Stderr, "registering the scheme: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "building the client: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}

// assetsDir finds the API server and etcd binaries.
//
// KUBEBUILDER_ASSETS wins when it is set, which is how `make test` passes them.
// The fallback is what makes a plain `go test ./...` work without remembering
// to go through the Makefile.
func assetsDir() string {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	root := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	suffix := fmt.Sprintf("-%s-%s", runtime.GOOS, runtime.GOARCH)
	for _, e := range entries {
		if e.IsDir() && filepath.Ext(e.Name()) != ".tmp" &&
			len(e.Name()) > len(suffix) && e.Name()[len(e.Name())-len(suffix):] == suffix {
			return filepath.Join(root, e.Name())
		}
	}
	return ""
}
