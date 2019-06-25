/*
Copyright 2018 The Kubernetes Authors.

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

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/klog"
	testutils "sigs.k8s.io/gcp-compute-persistent-disk-csi-driver/test/e2e/utils"
)

var (
	// Kubernetes cluster flags
	teardownCluster  = flag.Bool("teardown-cluster", true, "teardown the cluster after the e2e test")
	teardownDriver   = flag.Bool("teardown-driver", true, "teardown the driver after the e2e test")
	bringupCluster   = flag.Bool("bringup-cluster", true, "build kubernetes and bringup a cluster")
	gceZone          = flag.String("gce-zone", "", "zone that the gce k8s cluster is created/found in")
	kubeVersion      = flag.String("kube-version", "", "version of Kubernetes to download and use for the cluster")
	testVersion      = flag.String("test-version", "", "version of Kubernetes to download and use for tests")
	kubeFeatureGates = flag.String("kube-feature-gates", "", "feature gates to set on new kubernetes cluster")
	localK8sDir      = flag.String("local-k8s-dir", "", "local kubernetes/kubernetes directory to run e2e tests from")
	deploymentStrat  = flag.String("deployment-strategy", "", "choose between deploying on gce or gke")
	gkeClusterVer    = flag.String("gke-cluster-version", "", "version of Kubernetes master and node for gke")

	// Test infrastructure flags
	boskosResourceType = flag.String("boskos-resource-type", "gce-project", "name of the boskos resource type to reserve")
	storageClassFile   = flag.String("storageclass-file", "", "name of storageclass yaml file to use for test relative to test/k8s-integration/config")
	inProw             = flag.Bool("run-in-prow", false, "is the test running in PROW")

	// Driver flags
	stagingImage      = flag.String("staging-image", "", "name of image to stage to")
	saFile            = flag.String("service-account-file", "", "path of service account file")
	deployOverlayName = flag.String("deploy-overlay-name", "", "which kustomize overlay to deploy the driver with")
	doDriverBuild     = flag.Bool("do-driver-build", true, "building the driver from source")

	// Test flags
	migrationTest = flag.Bool("migration-test", false, "sets the flag on the e2e binary signalling migration")
	testFocus     = flag.String("test-focus", "", "test focus for Kubernetes e2e")
)

const (
	pdImagePlaceholder = "gke.gcr.io/gcp-compute-persistent-disk-csi-driver"
	k8sBuildBinDir     = "_output/dockerized/bin/linux/amd64"
	gkeTestClusterName = "gcp-pd-csi-driver-test-cluster"
)

func init() {
	flag.Set("logtostderr", "true")
}

func main() {
	flag.Parse()

	if !*inProw {
		ensureVariable(stagingImage, true, "staging-image is a required flag, please specify the name of image to stage to")
	}

	ensureVariable(saFile, true, "service-account-file is a required flag")
	ensureVariable(deployOverlayName, true, "deploy-overlay-name is a required flag")
	ensureVariable(testFocus, true, "test-focus is a required flag")
	ensureVariable(gceZone, true, "gce-zone is a required flag")

	if *migrationTest {
		ensureVariable(storageClassFile, false, "storage-class-file and migration-test cannot both be set")
	} else {
		ensureVariable(storageClassFile, true, "One of storageclass-file and migration-test must be set")
	}

	if !*bringupCluster {
		ensureVariable(kubeFeatureGates, false, "kube-feature-gates set but not bringing up new cluster")
	}

	if *bringupCluster || *teardownCluster {
		ensureVariable(deploymentStrat, true, "Must set the deployment strategy if bringing up or down cluster.")
	} else {
		ensureVariable(deploymentStrat, false, "Cannot set the deployment strategy if not bringing up or down cluster.")
	}

	if *deploymentStrat == "gke" {
		ensureFlag(migrationTest, false, "Cannot set deployment strategy to 'gke' for migration tests.")
		ensureVariable(kubeVersion, false, "Cannot set kube-version when using deployment strategy 'gke'. Use gke-cluster-version.")
		ensureVariable(gkeClusterVer, true, "Must set gke-cluster-version when using deployment strategy 'gke'.")
		ensureVariable(kubeFeatureGates, false, "Cannot set feature gates when using deployment strategy 'gke'.")
		if len(*localK8sDir) == 0 {
			ensureVariable(testVersion, true, "Must set either test-version or local k8s dir when using deployment strategy 'gke'.")
		}
	}

	if len(*localK8sDir) != 0 {
		ensureVariable(kubeVersion, false, "Cannot set a kube version when using a local k8s dir.")
		ensureVariable(testVersion, false, "Cannot set a test version when using a local k8s dir.")
	}

	err := handle()
	if err != nil {
		klog.Fatalf("Failed to run integration test: %v", err)
	}
}

func handle() error {
	oldmask := syscall.Umask(0000)
	defer syscall.Umask(oldmask)

	stagingVersion := string(uuid.NewUUID())

	goPath, ok := os.LookupEnv("GOPATH")
	if !ok {
		return fmt.Errorf("Could not find env variable GOPATH")
	}

	pkgDir := filepath.Join(goPath, "src", "sigs.k8s.io", "gcp-compute-persistent-disk-csi-driver")

	// If running in Prow, then acquire and set up a project through Boskos
	if *inProw {
		project, _ := testutils.SetupProwConfig(*boskosResourceType)

		oldProject, err := exec.Command("gcloud", "config", "get-value", "project").CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to get gcloud project: %s, err: %v", oldProject, err)
		}

		err = setEnvProject(project)
		if err != nil {
			return fmt.Errorf("failed to set project environment to %s: %v", project, err)
		}
		defer func() {
			err = setEnvProject(string(oldProject))
			if err != nil {
				klog.Errorf("failed to set project environment to %s: %v", oldProject, err)
			}
		}()

		if *doDriverBuild {
			*stagingImage = fmt.Sprintf("gcr.io/%s/gcp-persistent-disk-csi-driver", project)
		}

		if _, ok := os.LookupEnv("USER"); !ok {
			err = os.Setenv("USER", "prow")
			if err != nil {
				return fmt.Errorf("failed to set user in prow to prow: %v", err)
			}
		}
	}

	// Create temporary directories for kubernetes builds
	k8sParentDir := getTempDir()
	k8sDir := filepath.Join(k8sParentDir, "kubernetes")
	testParentDir := getTempDir()
	testDir := filepath.Join(testParentDir, "kubernetes")
	defer removeTempDir(k8sParentDir)
	defer removeTempDir(testParentDir)

	numTasks := 4
	errChan := make(chan error, numTasks)
	k8sDependencyChan := make(chan bool, 1)

	// If kube version is set, then download and build Kubernetes for cluster creation
	// Otherwise, either GKE or a prebuild local K8s dir is being used
	if len(*kubeVersion) != 0 {
		go func() {
			err := downloadKubernetesSource(pkgDir, k8sParentDir, *kubeVersion)
			if err != nil {
				errChan <- fmt.Errorf("failed to download Kubernetes source: %v", err)
				k8sDependencyChan <- false
				return
			}
			err = buildKubernetes(k8sDir)
			if err != nil {
				errChan <- fmt.Errorf("failed to build Kubernetes: %v", err)
				k8sDependencyChan <- false
				return
			}
			k8sDependencyChan <- true
			errChan <- nil
		}()
	} else {
		errChan <- nil
		k8sDir = *localK8sDir
		k8sDependencyChan <- true
	}

	// If test version is set, then download and build Kubernetes to run K8s tests
	// Otherwise, either kube version is set (which implies GCE) or a local K8s dir is being used
	if len(*testVersion) != 0 && *testVersion != *kubeVersion {
		go func() {
			// TODO: Build only the tests
			err := downloadKubernetesSource(pkgDir, testParentDir, *testVersion)
			if err != nil {
				errChan <- fmt.Errorf("failed to download Kubernetes source: %v", err)
				return
			}
			err = buildKubernetes(testDir)
			if err != nil {
				errChan <- fmt.Errorf("failed to build Kubernetes: %v", err)
				return
			}
			errChan <- nil
		}()
	} else {
		testDir = k8sDir
		errChan <- nil
	}

	// Build and push the driver, if required. Defer the driver image deletion.
	if *doDriverBuild {
		go func() {
			errChan <- pushImage(pkgDir, *stagingImage, stagingVersion)
		}()
		defer func() {
			if *teardownCluster {
				err := deleteImage(*stagingImage, stagingVersion)
				if err != nil {
					klog.Errorf("failed to delete image: %v", err)
				}
			}
		}()
	} else {
		errChan <- nil
	}

	if *bringupCluster {
		go func() {
			if !(<-k8sDependencyChan) {
				errChan <- nil
				return
			}

			switch *deploymentStrat {
			case "gce":
				kshPath := filepath.Join(k8sDir, "cluster", "kubectl.sh")
				_, err := os.Stat(kshPath)
				if err == nil {
					// Set kubectl to the one bundled in the k8s tar for versioning
					err = os.Setenv("GCE_PD_KUBECTL", kshPath)
					if err != nil {
						errChan <- fmt.Errorf("failed to set cluster specific kubectl: %v", err)
						return
					}
				} else {
					klog.Errorf("could not find cluster kubectl at %s, falling back to default kubectl", kshPath)
				}

				if len(*kubeFeatureGates) != 0 {
					err = os.Setenv("KUBE_FEATURE_GATES", *kubeFeatureGates)
					if err != nil {
						errChan <- fmt.Errorf("failed to set kubernetes feature gates: %v", err)
						return
					}
					klog.V(4).Infof("Set Kubernetes feature gates: %v", *kubeFeatureGates)
				}

				err = clusterUpGCE(k8sDir, *gceZone)
				if err != nil {
					errChan <- fmt.Errorf("failed to cluster up: %v", err)
					return
				}
			case "gke":
				err := clusterUpGKE(*gceZone)
				if err != nil {
					errChan <- fmt.Errorf("failed to cluster up: %v", err)
					return
				}
			default:
				errChan <- fmt.Errorf("deployment-strategy must be set to 'gce' or 'gke', but is: %s", *deploymentStrat)
			}
			errChan <- nil
		}()
	} else {
		errChan <- nil
	}

	// Defer the tear down of the cluster through GKE or GCE
	if *teardownCluster {
		defer func() {
			switch *deploymentStrat {
			case "gce":
				err := clusterDownGCE(k8sDir)
				if err != nil {
					klog.Errorf("failed to cluster down: %v", err)
				}
			case "gke":
				err := clusterDownGKE(*gceZone)
				if err != nil {
					klog.Errorf("failed to cluster down: %v", err)
				}
			default:
				klog.Errorf("deployment-strategy must be set to 'gce' or 'gke', but is: %s", *deploymentStrat)
			}
		}()
	}

	// Block until all background operations are complete
	var firstErr error = nil
	for i := 0; i < 4; i++ {
		if err := <-errChan; err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return firstErr
	}

	// Install the driver and defer its teardown
	err := installDriver(goPath, pkgDir, *stagingImage, stagingVersion, *deployOverlayName, *doDriverBuild)
	if *teardownDriver {
		defer func() {
			// TODO (#140): collect driver logs
			if teardownErr := deleteDriver(goPath, pkgDir, *deployOverlayName); teardownErr != nil {
				klog.Errorf("failed to delete driver: %v", teardownErr)
			}
		}()
	}
	if err != nil {
		return fmt.Errorf("failed to install CSI Driver: %v", err)
	}

	// Run the tests using the testDir kubernetes
	if len(*storageClassFile) != 0 {
		err = runCSITests(pkgDir, testDir, *testFocus, *storageClassFile, *gceZone)
	} else if *migrationTest {
		err = runMigrationTests(pkgDir, testDir, *testFocus, *gceZone)
	} else {
		return fmt.Errorf("Did not run either CSI or Migration test")
	}

	if err != nil {
		return fmt.Errorf("failed to run tests: %v", err)
	}

	return nil
}

func setEnvProject(project string) error {
	out, err := exec.Command("gcloud", "config", "set", "project", project).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to set gcloud project to %s: %s, err: %v", project, out, err)
	}

	err = os.Setenv("PROJECT", project)
	if err != nil {
		return err
	}
	return nil
}

func runMigrationTests(pkgDir, k8sDir, testFocus, gceZone string) error {
	return runTestsWithConfig(pkgDir, k8sDir, gceZone, testFocus, "-storage.migratedPlugins=kubernetes.io/gce-pd")
}

func runCSITests(pkgDir, k8sDir, testFocus, storageClassFile, gceZone string) error {
	testDriverConfigFile, err := generateDriverConfigFile(pkgDir, storageClassFile)
	if err != nil {
		return err
	}
	testConfigArg := fmt.Sprintf("-storage.testdriver=%s", testDriverConfigFile)
	return runTestsWithConfig(pkgDir, k8sDir, gceZone, testFocus, testConfigArg)
}

func runTestsWithConfig(pkgDir, k8sDir, gceZone, testFocus, testConfigArg string) error {
	err := os.Chdir(k8sDir)
	if err != nil {
		return err
	}

	homeDir, _ := os.LookupEnv("HOME")
	os.Setenv("KUBECONFIG", filepath.Join(homeDir, ".kube/config"))

	artifactsDir, _ := os.LookupEnv("ARTIFACTS")
	reportArg := fmt.Sprintf("-report-dir=%s", artifactsDir)

	testFocusArg := fmt.Sprintf("-focus=%s", testFocus)

	cmd := exec.Command(filepath.Join(k8sBuildBinDir, "ginkgo"),
		"-p",
		testFocusArg,
		"-skip=\\[Disruptive\\]|\\[Serial\\]|\\[Feature:.+\\]",
		filepath.Join(k8sBuildBinDir, "e2e.test"),
		"--",
		reportArg,
		"-provider=gce",
		"-node-os-distro=cos",
		fmt.Sprintf("-gce-zone=%s", gceZone),
		testConfigArg)

	err = runCommand("Running Tests", cmd)
	if err != nil {
		return fmt.Errorf("failed to run tests on e2e cluster: %v", err)
	}

	return nil
}
