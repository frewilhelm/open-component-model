package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/test/utils"
	"ocm.software/open-component-model/bindings/go/runtime"
)

const (
	localizationExample = "discovery-localization"
	localizationPrefix  = "ocm.software/ocm-k8s-toolkit/examples/" + localizationExample
	chartsSubPath       = "discovery-localization-charts"
)

// Transfer each CV separately: recursively transferring the root would also
// put the chart CVs in repository A and hide broken cross-repository routing.
func transferAcrossRepositories(ctx context.Context, constructorPath, registry string) error {
	ocm := utils.OCMBinary()
	tmpDir := GinkgoT().TempDir()
	ctfDir := filepath.Join(tmpDir, "ctf")
	add := exec.CommandContext(ctx, ocm, "add", "cv", "--repository", ctfDir, "--constructor", constructorPath)
	add.Dir = filepath.Dir(constructorPath)
	if _, err := utils.Run(add); err != nil {
		return fmt.Errorf("could not create ocm components: %w", err)
	}
	uploaderConfig := filepath.Join(filepath.Dir(constructorPath), "uploader.yaml")
	for _, suffix := range []string{"", "/rgd", "/platform", "/chart-webservice", "/chart-podinfo"} {
		target := registry
		args := []string{"transfer", "cv", fmt.Sprintf("ctf::%s//%s%s:1.0.0", ctfDir, localizationPrefix, suffix)}
		if strings.HasPrefix(suffix, "/chart-") {
			target += "/" + chartsSubPath
			args = append(args, target, "--copy-resources", "--config", uploaderConfig)
		} else {
			args = append(args, target)
		}
		By("transferring " + localizationPrefix + suffix + " to " + target)
		if _, err := utils.Run(exec.CommandContext(ctx, ocm, args...)); err != nil {
			return err
		}
	}
	return nil
}

func localizationObject(ctx context.Context, resource string, into any) {
	GinkgoHelper()
	out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "get", resource, "-n", "default", "-o", "json"))
	Expect(err).NotTo(HaveOccurred())
	Expect(json.Unmarshal(out, into)).To(Succeed())
}

func localizationReady(ctx context.Context, resource string) {
	GinkgoHelper()
	Expect(utils.WaitForResource(ctx, "create", timeout, resource)).To(Succeed())
	Expect(utils.WaitForResource(ctx, "condition=Ready=true", timeout, resource)).To(Succeed())
}

var _ = Describe("Discovery Cross-Repository Localization Tests", func() {
	It("localizes multiple images into different chart layouts using declared value paths", func(ctx SpecContext) {
		exampleDir := filepath.Join(examplesDir, localizationExample)
		Expect(transferAcrossRepositories(ctx, filepath.Join(exampleDir, ComponentConstructor), imageRegistry)).To(Succeed())

		By("proving the chart CVs exist only in repository B")
		for _, leaf := range []string{"chart-webservice", "chart-podinfo"} {
			for _, check := range []struct {
				prefix string
				status int
			}{{"", http.StatusNotFound}, {chartsSubPath + "/", http.StatusOK}} {
				url := imageRegistry + "/v2/" + check.prefix + "component-descriptors/" + localizationPrefix + "/" + leaf + "/manifests/1.0.0"
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				Expect(err).NotTo(HaveOccurred())
				req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json")
				resp, err := http.DefaultClient.Do(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Body.Close()).To(Succeed())
				Expect(resp.StatusCode).To(Equal(check.status), url)
			}
		}

		Expect(utils.DeployResource(ctx, filepath.Join(exampleDir, Bootstrap))).To(Succeed())
		localizationReady(ctx, "rgd/"+localizationExample)
		Expect(utils.DeployResource(ctx, filepath.Join(exampleDir, Instance))).To(Succeed())

		repoB := strings.TrimPrefix(strings.TrimPrefix(imageRegistry, "http://"), "https://") + "/" + chartsSubPath + "/"
		digests := map[string]string{
			"6.9.1": "262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb",
			"6.9.0": "10af81c9659824cecaa7fe09134c6c318136341b4ae84c4a37f18d9bd8a33eac",
			"6.7.1": "862ca45e61b32392f7941a1bdfdbe5ff8b6899070135f1bdca1c287d0057fc94",
		}
		type imageCase struct {
			name, tag, valuesPath, workload string
		}
		for _, chart := range []struct {
			leaf   string
			images []imageCase
		}{
			{"chart-webservice", []imageCase{
				{"image-main", "6.9.1", "main.image", "-webservice"},
				{"image-canary", "6.9.0", "worker.image", "-worker"},
				{"image-unmapped", "6.7.1", "", ""},
			}},
			{"chart-podinfo", []imageCase{{"image-main", "6.7.1", "image", ""}}},
		} {
			chartName := localizationExample + "-" + chart.leaf
			path := []runtime.Identity{{"name": "platform"}, {"name": chart.leaf}}
			By("verifying chart and image Resources for " + chart.leaf)
			localizationReady(ctx, "resource.delivery.ocm.software/"+chartName)
			var chartResource v1alpha1.Resource
			localizationObject(ctx, "resource.delivery.ocm.software/"+chartName, &chartResource)
			Expect(chartResource.Spec.Resource.ByReference.ReferencePath).To(Equal(path))
			Expect(chartResource.Status.Component.Component).To(Equal(localizationPrefix + "/" + chart.leaf))
			var chartAccess struct {
				Registry   string `json:"registry"`
				Repository string `json:"repository"`
			}
			Expect(json.Unmarshal(chartResource.Status.Additional.Raw, &chartAccess)).To(Succeed())
			Expect(chartAccess.Registry + "/" + chartAccess.Repository).To(HavePrefix(repoB))

			var references []string
			var wantValuesFrom []map[string]string
			for _, img := range chart.images {
				name := localizationExample + "-image-" + chart.leaf + "-" + img.name
				res := "resource.delivery.ocm.software/" + name
				localizationReady(ctx, res)
				var resource v1alpha1.Resource
				localizationObject(ctx, res, &resource)
				Expect(resource.Spec.Resource.ByReference.ReferencePath).To(Equal(path))
				Expect(resource.Status.Component.Component).To(Equal(localizationPrefix + "/" + chart.leaf))
				Expect(resource.Status.Resource.Name).To(Equal(img.name))
				Expect(resource.Status.Resource.Version).To(Equal(img.tag))
				var status struct {
					Reference  string              `json:"reference"`
					Registry   string              `json:"registry"`
					Repository string              `json:"repository"`
					Tag        string              `json:"tag"`
					Targets    []map[string]string `json:"targets"`
				}
				Expect(json.Unmarshal(resource.Status.Additional.Raw, &status)).To(Succeed())
				wantImage := repoB + "stefanprodan/podinfo:" + img.tag
				normalized := strings.TrimPrefix(strings.TrimPrefix(status.Reference, "http://"), "https://")
				Expect(normalized).To(Equal(wantImage + "@sha256:" + digests[img.tag]))
				Expect(status.Registry + "/" + status.Repository + ":" + status.Tag).To(Equal(wantImage))
				references = append(references, status.Reference)
				if img.valuesPath == "" {
					Expect(status.Targets).To(BeEmpty(), "unlabeled image is status-only")
					continue
				}
				Expect(status.Targets).To(Equal([]map[string]string{{"repository": img.valuesPath + ".repository", "tag": img.valuesPath + ".tag"}}))
				for _, key := range []string{"repository", "tag"} {
					wantValuesFrom = append(wantValuesFrom, map[string]string{
						"kind": "ConfigMap", "name": name + "-values", "valuesKey": key, "targetPath": img.valuesPath + "." + key,
					})
				}
			}

			By("checking the exact valuesFrom mapping, with no entry for unlabeled images")
			localizationReady(ctx, "helmrelease/"+chartName+"-hr")
			var release struct {
				Spec struct {
					ValuesFrom []map[string]string `json:"valuesFrom"`
				} `json:"spec"`
			}
			localizationObject(ctx, "helmrelease/"+chartName+"-hr", &release)
			Expect(release.Spec.ValuesFrom).To(ConsistOf(wantValuesFrom))
			Expect(utils.WaitForResource(ctx, "create", timeout, "configmap/"+chartName+"-images")).To(Succeed())
			var summary corev1.ConfigMap
			localizationObject(ctx, "configmap/"+chartName+"-images", &summary)
			Expect(strings.Split(summary.Data["images"], ",")).To(ConsistOf(references))

			By("checking actual Ready Pods use each independently expected localized image")
			for _, img := range chart.images {
				if img.valuesPath == "" {
					continue
				}
				dep := "deployment/" + chartName + img.workload
				Expect(utils.WaitForResource(ctx, "condition=Available", timeout, dep)).To(Succeed())
				var deployment appsv1.Deployment
				localizationObject(ctx, dep, &deployment)
				selector := metav1.FormatLabelSelector(deployment.Spec.Selector)
				Expect(utils.WaitForResource(ctx, "condition=Ready", timeout, "pod", "-l", selector)).To(Succeed())
				out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "get", "pods", "-n", "default", "-l", selector, "-o", "json"))
				Expect(err).NotTo(HaveOccurred())
				var pods corev1.PodList
				Expect(json.Unmarshal(out, &pods)).To(Succeed())
				Expect(pods.Items).NotTo(BeEmpty())
				for _, pod := range pods.Items {
					Expect(pod.Spec.Containers[0].Image).To(Equal(repoB + "stefanprodan/podinfo:" + img.tag))
					Expect(pod.Status.ContainerStatuses[0].Ready).To(BeTrue())
				}
			}
		}
	})
})
