package e2e

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
	localizationExample  = "discovery-localization"
	localizationRoot     = "example.com/observability-demo"
	localizationProducts = "example.com/products/"
	productsSubPath      = "discovery-localization-products"
)

// transferAcrossRepositories builds the component graph and transfers product CVs to
// repository B (products subpath) and the root CV to repository A. Never transfers
// recursively: keeping products out of A exercises the cross-repository routing.
func transferAcrossRepositories(ctx context.Context, constructorPath, registry string) error {
	// Pull/refresh pinned chart archives via the example Taskfile.
	exampleDir := filepath.Dir(constructorPath)
	prepareCmd := exec.CommandContext(ctx, "task", "--dir", exampleDir, "prepare")
	if _, err := utils.Run(prepareCmd); err != nil {
		return fmt.Errorf("could not prepare chart archives: %w", err)
	}

	ocm := utils.OCMBinary()
	tmpDir := GinkgoT().TempDir()
	ctfDir := filepath.Join(tmpDir, "ctf")
	add := exec.CommandContext(ctx, ocm, "add", "cv", "--repository", ctfDir, "--constructor", constructorPath)
	add.Dir = exampleDir
	if _, err := utils.Run(add); err != nil {
		return fmt.Errorf("could not create ocm components: %w", err)
	}

	uploaderConfig := filepath.Join(exampleDir, "uploader.yaml")
	repoB := registry + "/" + productsSubPath

	// Transfer three product CVs to repository B with --copy-resources.
	for _, cv := range []struct {
		name    string
		version string
	}{
		{localizationProducts + "podinfo", "6.9.1"},
		{localizationProducts + "prometheus", "29.47.0"},
		{localizationProducts + "grafana", "14.4.0"},
	} {
		By(fmt.Sprintf("transferring %s:%s to repository B", cv.name, cv.version))
		args := []string{"transfer", "cv",
			fmt.Sprintf("ctf::%s//%s:%s", ctfDir, cv.name, cv.version),
			repoB, "--copy-resources", "--config", uploaderConfig,
		}
		if _, err := utils.Run(exec.CommandContext(ctx, ocm, args...)); err != nil {
			return fmt.Errorf("could not transfer %s:%s: %w", cv.name, cv.version, err)
		}
	}

	// Transfer root CV to repository A only (not recursive).
	By("transferring root CV to repository A")
	args := []string{"transfer", "cv",
		fmt.Sprintf("ctf::%s//%s:1.0.0", ctfDir, localizationRoot),
		registry,
	}
	if _, err := utils.Run(exec.CommandContext(ctx, ocm, args...)); err != nil {
		return fmt.Errorf("could not transfer root CV: %w", err)
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

// localizationServiceURL starts a kubectl port-forward for the given service and port,
// detects the assigned local port, and registers process cleanup via DeferCleanup.
// Returns the base URL "http://127.0.0.1:<localport>".
func localizationServiceURL(ctx context.Context, service string, port int) (string, error) {
	GinkgoHelper()
	cmd := exec.CommandContext(ctx, "kubectl", "-n", "default", "port-forward",
		"--address", "127.0.0.1",
		"service/"+service,
		fmt.Sprintf(":%d", port),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("port-forward stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("port-forward start: %w", err)
	}
	DeferCleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Detect "Forwarding from 127.0.0.1:<localport>" within 30 s.
	assignedPort := ""
	ready := make(chan struct{})
	go func() {
		defer close(ready)
		scanner := bufio.NewScanner(stdout)
		re := regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)
		for scanner.Scan() {
			if m := re.FindStringSubmatch(scanner.Text()); m != nil {
				assignedPort = m[1]
				return
			}
		}
	}()

	// Check for early process exit while waiting.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case <-ready:
		if assignedPort == "" {
			return "", fmt.Errorf("port-forward did not emit a local port")
		}
	case err := <-exited:
		return "", fmt.Errorf("port-forward process exited early: %w", err)
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("port-forward did not become ready within 30s")
	}
	return "http://127.0.0.1:" + assignedPort, nil
}

var _ = Describe("Discovery Cross-Repository Localization Tests", func() {
	It("deploys a verified product release and serves podinfo metrics through Grafana", func(ctx SpecContext) {
		exampleDir := filepath.Join(examplesDir, localizationExample)
		Expect(transferAcrossRepositories(ctx, filepath.Join(exampleDir, ComponentConstructor), imageRegistry)).To(Succeed())

		// Verified split: product CVs must be in B only; root must be in A.
		repoB := strings.TrimPrefix(strings.TrimPrefix(imageRegistry, "http://"), "https://") + "/" + productsSubPath
		for _, product := range []struct {
			name    string
			version string
		}{
			{"podinfo", "6.9.1"},
			{"prometheus", "29.47.0"},
			{"grafana", "14.4.0"},
		} {
			By("verifying " + product.name + " is absent from A and present in B")
			for _, check := range []struct {
				prefix string
				status int
			}{
				{"", http.StatusNotFound},
				{productsSubPath + "/", http.StatusOK},
			} {
				url := imageRegistry + "/v2/" + check.prefix + "component-descriptors/" +
					localizationProducts + product.name + "/manifests/" + product.version
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				Expect(err).NotTo(HaveOccurred())
				req.Header.Set("Accept",
					"application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json")
				resp, err := http.DefaultClient.Do(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Body.Close()).To(Succeed())
				Expect(resp.StatusCode).To(Equal(check.status), url)
			}
		}

		By("verifying root is present in A")
		{
			url := imageRegistry + "/v2/component-descriptors/" + localizationRoot + "/manifests/1.0.0"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Accept",
				"application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Body.Close()).To(Succeed())
			Expect(resp.StatusCode).To(Equal(http.StatusOK), url)
		}

		Expect(utils.DeployResource(ctx, filepath.Join(exampleDir, Bootstrap))).To(Succeed())
		localizationReady(ctx, "component/discovery-localization-component")
		Expect(utils.DeployResource(ctx, filepath.Join(exampleDir, Rgd))).To(Succeed())
		localizationReady(ctx, "rgd/"+localizationExample)
		Expect(utils.WaitForResource(ctx, "condition=Established", timeout,
			"crd/discoverylocalizations.kro.run")).To(Succeed())
		Expect(utils.DeployResource(ctx, filepath.Join(exampleDir, Instance))).To(Succeed())

		// Fixed digest table for cross-check. Verify against these constants,
		// not against another field derived from the same Resource.
		pinnedDigests := map[string]string{
			"podinfo-image":                    "sha256:a40fbec5a9caea1879669ce59f9d3a08b68bccfd46fc9760994275c3ca6b4d0f",
			"prometheus-server-image":          "sha256:89c1a644817e9ad70fa2af76c84898e327c1de7063735f962e8313b1e28672c3",
			"prometheus-config-reloader-image": "sha256:15f6aec87a20e25f33371ebf9f37bfba33d783f8f7c7dcc3448720c1a5397c2c",
			"grafana-image":                    "sha256:ffa50e781972f7862410639eed96e0a9ad96c94ebeccd43e634141f3d9605a99",
		}

		prefix := "discovery-localization"

		// --- Chart Resources ---
		for _, chart := range []struct {
			product string
			version string
			alias   string
		}{
			{"podinfo", "6.9.1", "podinfo"},
			{"prometheus", "29.47.0", "prometheus"},
			{"grafana", "14.4.0", "grafana"},
		} {
			name := prefix + "-" + chart.product + "-chart"
			By("verifying chart Resource for " + chart.product)
			localizationReady(ctx, "resource.delivery.ocm.software/"+name)

			var res v1alpha1.Resource
			localizationObject(ctx, "resource.delivery.ocm.software/"+name, &res)

			// One-hop reference path from root to the product alias.
			wantPath := []runtime.Identity{{"name": chart.alias}}
			Expect(res.Spec.Resource.ByReference.ReferencePath).To(Equal(wantPath))
			Expect(res.Status.Component.Component).To(Equal(localizationProducts + chart.product))
			Expect(res.Status.Resource.Version).To(Equal(chart.version))

			// Chart must resolve into repository B with a non-empty digest.
			var chartAccess struct {
				Registry   string `json:"registry"`
				Repository string `json:"repository"`
				Digest     string `json:"digest"`
			}
			Expect(json.Unmarshal(res.Status.Additional.Raw, &chartAccess)).To(Succeed())
			Expect(chartAccess.Registry+"/"+chartAccess.Repository).To(HavePrefix(repoB+"/"))
			Expect(chartAccess.Digest).To(HavePrefix("sha256:"))
		}

		// --- Image Resources ---
		for _, img := range []struct {
			product      string
			resourceName string
			tag          string
		}{
			{"podinfo", "image", "6.9.1"},
			{"prometheus", "server-image", "v3.16.0"},
			{"prometheus", "config-reloader-image", "v0.95.1"},
			{"grafana", "image", "13.3.0-distroless"},
		} {
			name := prefix + "-" + img.product + "-" + img.resourceName
			By("verifying image Resource for " + name)
			localizationReady(ctx, "resource.delivery.ocm.software/"+name)

			var res v1alpha1.Resource
			localizationObject(ctx, "resource.delivery.ocm.software/"+name, &res)

			wantPath := []runtime.Identity{{"name": img.product}}
			Expect(res.Spec.Resource.ByReference.ReferencePath).To(Equal(wantPath))
			Expect(res.Status.Component.Component).To(Equal(localizationProducts + img.product))
			Expect(res.Status.Resource.Name).To(Equal(img.resourceName))
			Expect(res.Status.Resource.Version).To(Equal(img.tag))

			var status struct {
				Reference  string `json:"reference"`
				Registry   string `json:"registry"`
				Repository string `json:"repository"`
				Tag        string `json:"tag"`
				Digest     string `json:"digest"`
			}
			Expect(json.Unmarshal(res.Status.Additional.Raw, &status)).To(Succeed())

			// Image must be resolved into repo B (not upstream).
			Expect(status.Registry+"/"+status.Repository).To(HavePrefix(repoB+"/"))
			Expect(status.Tag).To(Equal(img.tag))

			// Check digest against the pinned constant, not a derived field.
			key := img.product + "-" + img.resourceName
			Expect(status.Digest).To(Equal(pinnedDigests[key]),
				"digest mismatch for "+name)
		}

		// Wait for instance and all three HelmReleases.
		localizationReady(ctx, "discoverylocalization/discovery-localization")
		for _, product := range []string{"podinfo", "prometheus", "grafana"} {
			localizationReady(ctx, "helmrelease/"+prefix+"-"+product+"-hr")
		}

		// --- Workload assertions ---
		deployments := map[string][]string{
			prefix + "-podinfo":     {"podinfo"},
			prefix + "-prometheus":  {"prometheus-server", "prometheus-server-configmap-reload"},
			prefix + "-grafana":     {"grafana"},
		}
		for depName, containers := range deployments {
			By("checking deployment " + depName)
			dep := "deployment/" + depName
			Expect(utils.WaitForResource(ctx, "condition=Available", timeout, dep)).To(Succeed())
			var deployment appsv1.Deployment
			localizationObject(ctx, dep, &deployment)
			selector := metav1.FormatLabelSelector(deployment.Spec.Selector)
			Expect(utils.WaitForResource(ctx, "condition=Ready", timeout, "pod", "-l", selector)).To(Succeed())

			out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "get", "pods",
				"-n", "default", "-l", selector, "-o", "json"))
			Expect(err).NotTo(HaveOccurred())
			var pods corev1.PodList
			Expect(json.Unmarshal(out, &pods)).To(Succeed())
			Expect(pods.Items).NotTo(BeEmpty())

			for _, pod := range pods.Items {
				// Match containers by name, not index.
				podContainers := map[string]corev1.Container{}
				for _, c := range pod.Spec.Containers {
					podContainers[c.Name] = c
				}
				for _, cName := range containers {
					c, ok := podContainers[cName]
					Expect(ok).To(BeTrue(), "container %q not found in pod %s", cName, pod.Name)
					// Container image must originate from repo B (not upstream registry).
					Expect(c.Image).To(ContainSubstring(repoB+"/"),
						"container %s image %s should be in repo B", cName, c.Image)
				}
				// Check no unexpected init containers or extra sidecars beyond the declared list.
				Expect(pod.Spec.InitContainers).To(BeEmpty(),
					"unexpected init containers in pod %s/%s", depName, pod.Name)

				// Check ready status for each container.
				statusMap := map[string]corev1.ContainerStatus{}
				for _, cs := range pod.Status.ContainerStatuses {
					statusMap[cs.Name] = cs
				}
				for _, cName := range containers {
					Expect(statusMap[cName].Ready).To(BeTrue(),
						"container %s not ready in pod %s", cName, pod.Name)
				}
			}
		}

		// Verify the exact mirror image strings for each product container.
		// Podinfo and Grafana render repo:tag@digest; the Prometheus chart renders
		// repo@digest (digest field suppresses the tag in the image string).
		for _, check := range []struct {
			deployment    string
			containerName string
			imageKey      string
			expectedTag   string
			checkTag      bool
		}{
			{prefix + "-podinfo", "podinfo", "podinfo-image", "6.9.1", true},
			{prefix + "-prometheus", "prometheus-server", "prometheus-server-image", "v3.16.0", false},
			{prefix + "-prometheus", "prometheus-server-configmap-reload", "prometheus-config-reloader-image", "v0.95.1", false},
			{prefix + "-grafana", "grafana", "grafana-image", "13.3.0-distroless", true},
		} {
			By("checking exact image string for " + check.containerName)
			var deployment appsv1.Deployment
			localizationObject(ctx, "deployment/"+check.deployment, &deployment)
			var containerImage string
			for _, c := range deployment.Spec.Template.Spec.Containers {
				if c.Name == check.containerName {
					containerImage = c.Image
					break
				}
			}
			Expect(containerImage).NotTo(BeEmpty(), "container %s not found", check.containerName)
			Expect(containerImage).To(ContainSubstring(repoB+"/"),
				"image %s should be in repo B", containerImage)
			if check.checkTag {
				Expect(containerImage).To(ContainSubstring(":"+check.expectedTag),
					"image %s should use tag %s", containerImage, check.expectedTag)
			}
			Expect(containerImage).To(ContainSubstring("@"+pinnedDigests[check.imageKey]),
				"image %s should carry pinned digest", containerImage)
		}

		// --- Live service checks ---

		By("port-forwarding podinfo and checking version")
		podinfSvc := prefix + "-podinfo"
		podinfoURL, err := localizationServiceURL(ctx, podinfSvc, 9898)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Get(podinfoURL + "/")
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
			g.Expect(string(body)).To(ContainSubstring("6.9.1"))
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("port-forwarding Prometheus and checking podinfo scrape target")
		promSvc := prefix + "-prometheus"
		promURL, err := localizationServiceURL(ctx, promSvc, 80)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Get(promURL + `/api/v1/query?query=up%7Bjob%3D%22podinfo%22%7D`)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var result struct {
				Status string `json:"status"`
				Data   struct {
					Result []struct {
						Value []any `json:"value"`
					} `json:"result"`
				} `json:"data"`
			}
			g.Expect(json.Unmarshal(body, &result)).To(Succeed())
			g.Expect(result.Status).To(Equal("success"))
			g.Expect(result.Data.Result).To(HaveLen(1),
				"expected one podinfo scrape target")
			val := fmt.Sprintf("%v", result.Data.Result[0].Value[1])
			g.Expect(val).To(Equal("1"), "podinfo scrape target should be up")
		}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		By("checking scrape_samples_scraped for podinfo is positive")
		Eventually(func(g Gomega) {
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Get(promURL + `/api/v1/query?query=scrape_samples_scraped%7Bjob%3D%22podinfo%22%7D`)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			var result struct {
				Status string `json:"status"`
				Data   struct {
					Result []struct {
						Value []any `json:"value"`
					} `json:"result"`
				} `json:"data"`
			}
			g.Expect(json.Unmarshal(body, &result)).To(Succeed())
			g.Expect(result.Status).To(Equal("success"))
			g.Expect(result.Data.Result).NotTo(BeEmpty())
			val := fmt.Sprintf("%v", result.Data.Result[0].Value[1])
			// Value should be a positive integer string.
			g.Expect(val).NotTo(Equal("0"))
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		By("port-forwarding Grafana and verifying health, dashboard and live datasource query")
		grafanaSvc := prefix + "-grafana"
		grafanaURL, err := localizationServiceURL(ctx, grafanaSvc, 80)
		Expect(err).NotTo(HaveOccurred())

		// Read credentials from the typed Secret — never from logs or command arguments.
		var grafanaSecret corev1.Secret
		localizationObject(ctx, "secret/"+prefix+"-grafana", &grafanaSecret)
		grafanaUser := string(grafanaSecret.Data["admin-user"])
		grafanaPass := string(grafanaSecret.Data["admin-password"])
		Expect(grafanaUser).NotTo(BeEmpty(), "Grafana admin-user secret key missing")
		Expect(grafanaPass).NotTo(BeEmpty(), "Grafana admin-password secret key missing")

		grafanaBasicAuth := "Basic " + base64.StdEncoding.EncodeToString(
			[]byte(grafanaUser+":"+grafanaPass))

		grafanaGet := func(path string) ([]byte, int, error) {
			client := &http.Client{Timeout: 10 * time.Second}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, grafanaURL+path, nil)
			if err != nil {
				return nil, 0, err
			}
			req.Header.Set("Authorization", grafanaBasicAuth)
			resp, err := client.Do(req)
			if err != nil {
				return nil, 0, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return body, resp.StatusCode, nil
		}

		By("checking Grafana health")
		Eventually(func(g Gomega) {
			body, status, err := grafanaGet("/api/health")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal(http.StatusOK))
			var health struct {
				Database string `json:"database"`
			}
			g.Expect(json.Unmarshal(body, &health)).To(Succeed())
			g.Expect(health.Database).To(Equal("ok"))
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		By("checking podinfo-observability dashboard is present with correct panels and datasource")
		Eventually(func(g Gomega) {
			body, status, err := grafanaGet("/api/dashboards/uid/podinfo-observability")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal(http.StatusOK))
			var dash struct {
				Dashboard struct {
					Panels []struct {
						ID         int    `json:"id"`
						Title      string `json:"title"`
						Datasource struct {
							UID string `json:"uid"`
						} `json:"datasource"`
					} `json:"panels"`
				} `json:"dashboard"`
			}
			g.Expect(json.Unmarshal(body, &dash)).To(Succeed())
			g.Expect(dash.Dashboard.Panels).To(HaveLen(2))
			panelTitles := make([]string, 0, 2)
			for _, p := range dash.Dashboard.Panels {
				panelTitles = append(panelTitles, p.Title)
				g.Expect(p.Datasource.UID).To(Equal("prometheus"))
			}
			g.Expect(panelTitles).To(ConsistOf(
				"Podinfo scrape availability",
				"Podinfo scraped samples",
			))
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("checking live Prometheus query through Grafana datasource proxy")
		Eventually(func(g Gomega) {
			body, status, err := grafanaGet(
				`/api/datasources/proxy/uid/prometheus/api/v1/query?query=up%7Bjob%3D%22podinfo%22%7D`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal(http.StatusOK))
			var result struct {
				Status string `json:"status"`
				Data   struct {
					Result []struct {
						Value []any `json:"value"`
					} `json:"result"`
				} `json:"data"`
			}
			g.Expect(json.Unmarshal(body, &result)).To(Succeed())
			g.Expect(result.Status).To(Equal("success"))
			g.Expect(result.Data.Result).To(HaveLen(1))
			val := fmt.Sprintf("%v", result.Data.Result[0].Value[1])
			g.Expect(val).To(Equal("1"))
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())

		By("checking scrape_samples_scraped through Grafana datasource proxy is positive")
		Eventually(func(g Gomega) {
			body, status, err := grafanaGet(
				`/api/datasources/proxy/uid/prometheus/api/v1/query?query=scrape_samples_scraped%7Bjob%3D%22podinfo%22%7D`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal(http.StatusOK))
			var result struct {
				Status string `json:"status"`
				Data   struct {
					Result []struct {
						Value []any `json:"value"`
					} `json:"result"`
				} `json:"data"`
			}
			g.Expect(json.Unmarshal(body, &result)).To(Succeed())
			g.Expect(result.Status).To(Equal("success"))
			g.Expect(result.Data.Result).NotTo(BeEmpty())
			val := fmt.Sprintf("%v", result.Data.Result[0].Value[1])
			g.Expect(val).NotTo(Equal("0"))
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
	})
})
