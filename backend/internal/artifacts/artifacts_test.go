package artifacts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// clusterTopDirHTML returns a minimal GCSweb listing for a cluster's top-level dirs.
func clusterTopDirHTML(clusterName string) string {
	return `<!DOCTYPE html><html><body><ul>
	<li><a href="../"> ..</a></li>
	<li><a href="azure-activity-logs/"> azure-activity-logs/</a></li>
	<li><a href="machines/"> machines/</a></li>
	<li><a href="kube-system/"> kube-system/</a></li>
	<li><a href="calico-system/"> calico-system/</a></li>
</ul></body></html>`
}

func TestDiscoverClusters(t *testing.T) {
	clustersHTML := loadFixture(t, "clusters_listing.html")
	machinesHTML := loadFixture(t, "machines_listing.html")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		path := r.URL.Path

		// Machine file listing (deepest path with a machine name)
		if strings.Contains(path, "/machines/") && !strings.HasSuffix(path, "/machines/") {
			w.Write([]byte(`<!DOCTYPE html><html><body>
			<ul class="resource-grid">
			<li class="pure-g grid-row"><div class="pure-u-2-5"><a href="../"><img src="/icons/back.png"> ..</a></div><div class="pure-u-1-5">-</div><div class="pure-u-2-5">-</div></li>
			<li class="pure-g grid-row"><div class="pure-u-2-5"><a href="boot.log"><img src="/icons/file.png"> boot.log</a></div><div class="pure-u-1-5">524288</div><div class="pure-u-2-5">20 Mar 2026</div></li>
			<li class="pure-g grid-row"><div class="pure-u-2-5"><a href="cloud-init-output.log"><img src="/icons/file.png"> cloud-init-output.log</a></div><div class="pure-u-1-5">0</div><div class="pure-u-2-5">20 Mar 2026</div></li>
			<li class="pure-g grid-row"><div class="pure-u-2-5"><a href="cloud-init.log"><img src="/icons/file.png"> cloud-init.log</a></div><div class="pure-u-1-5">0</div><div class="pure-u-2-5">20 Mar 2026</div></li>
			<li class="pure-g grid-row"><div class="pure-u-2-5"><a href="kubelet.log"><img src="/icons/file.png"> kubelet.log</a></div><div class="pure-u-1-5">1048576</div><div class="pure-u-2-5">20 Mar 2026</div></li>
			<li class="pure-g grid-row"><div class="pure-u-2-5"><a href="kube-apiserver.log"><img src="/icons/file.png"> kube-apiserver.log</a></div><div class="pure-u-1-5">2097152</div><div class="pure-u-2-5">20 Mar 2026</div></li>
			<li class="pure-g grid-row"><div class="pure-u-2-5"><a href="journal.log"><img src="/icons/file.png"> journal.log</a></div><div class="pure-u-1-5">0</div><div class="pure-u-2-5">20 Mar 2026</div></li>
			<li class="pure-g grid-row"><div class="pure-u-2-5"><a href="kern.log"><img src="/icons/file.png"> kern.log</a></div><div class="pure-u-1-5">0</div><div class="pure-u-2-5">20 Mar 2026</div></li>
			<li class="pure-g grid-row"><div class="pure-u-2-5"><a href="containerd.log"><img src="/icons/file.png"> containerd.log</a></div><div class="pure-u-1-5">0</div><div class="pure-u-2-5">20 Mar 2026</div></li>
			</ul></body></html>`))
			return
		}

		switch {
		case strings.HasSuffix(path, "/artifacts/clusters/"):
			w.Write(clustersHTML)
		case strings.HasSuffix(path, "/machines/"):
			w.Write(machinesHTML)
		case strings.Contains(path, "/azure-activity-logs/"):
			w.Write([]byte(`<!DOCTYPE html><html><body><ul>
			<li><a href="../"> ..</a></li>
			</ul></body></html>`))
		case strings.HasSuffix(path, "/capz-e2e-abc123-ha/") || strings.HasSuffix(path, "/capz-e2e-abc123-ipv6/"):
			w.Write([]byte(clusterTopDirHTML("")))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	listingBase := srv.URL + "/artifacts/clusters/"
	gcsBase := srv.URL + "/gcs/artifacts/clusters/"

	clusters, err := discoverClustersFromURL(context.Background(), srv.Client(), listingBase, gcsBase)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// bootstrap should be skipped
	for _, c := range clusters {
		if strings.EqualFold(c.ClusterName, "bootstrap") {
			t.Error("bootstrap cluster should be skipped")
		}
	}

	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(clusters))
	}

	// Check first cluster (ha)
	ha := clusters[0]
	if ha.ClusterName != "capz-e2e-abc123-ha" {
		t.Errorf("expected cluster name capz-e2e-abc123-ha, got %s", ha.ClusterName)
	}
	if ha.AzureActivityLog == "" {
		t.Error("expected azure activity log URL")
	}
	if len(ha.Machines) != 2 {
		t.Fatalf("expected 2 machines, got %d", len(ha.Machines))
	}
	if ha.Machines[0].Name != "capz-e2e-abc123-ha-control-plane-jkl42" {
		t.Errorf("unexpected machine name: %s", ha.Machines[0].Name)
	}
	if len(ha.Machines[0].Logs) != 3 {
		t.Errorf("expected 3 non-empty log entries, got %d: %v", len(ha.Machines[0].Logs), ha.Machines[0].Logs)
	}

	// Check pod log dirs
	if len(ha.PodLogDirs) != 2 {
		t.Fatalf("expected 2 pod log dirs, got %d: %v", len(ha.PodLogDirs), ha.PodLogDirs)
	}
	if _, ok := ha.PodLogDirs["kube-system"]; !ok {
		t.Error("expected kube-system in pod log dirs")
	}
}

func TestDiscoverClustersSkipsBootstrap(t *testing.T) {
	// HTML with only bootstrap
	html := `<!DOCTYPE html><html><body><ul>
		<li><a href="../"> ..</a></li>
		<li><a href="bootstrap/"> bootstrap/</a></li>
	</ul></body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(html))
	}))
	defer srv.Close()

	clusters, err := discoverClustersFromURL(context.Background(), srv.Client(), srv.URL+"/", srv.URL+"/gcs/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(clusters) != 0 {
		t.Errorf("expected 0 clusters (bootstrap only), got %d", len(clusters))
	}
}

func TestParseGCSWebDirs(t *testing.T) {
	input := `<!DOCTYPE html><html><body><ul>
		<li><a href="/some/path/"> ..</a></li>
		<li><a href="/some/path/dir-a/"> dir-a/</a></li>
		<li><a href="/some/path/dir-b/"> dir-b/</a></li>
		<li><a href="/some/path/file.txt">file.txt</a></li>
	</ul></body></html>`

	dirs, err := parseGCSWebDirs(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
	if dirs[0] != "dir-a" || dirs[1] != "dir-b" {
		t.Errorf("unexpected dirs: %v", dirs)
	}
}

func TestParseGCSWebDirsBackLink(t *testing.T) {
	input := `<!DOCTYPE html><html><body><ul>
		<li><a href="../"> ..</a></li>
	</ul></body></html>`

	dirs, err := parseGCSWebDirs(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dirs) != 0 {
		t.Errorf("expected 0 dirs (back link only), got %d", len(dirs))
	}
}

func TestMapTestToClusterSingleCluster(t *testing.T) {
	clusters := []models.ClusterArtifacts{
		{ClusterName: "capz-e2e-abc123-ha"},
	}

	result := MapTestToCluster("some random test name", clusters)
	if result == nil {
		t.Fatal("expected match for single cluster")
	}
	if result.ClusterName != "capz-e2e-abc123-ha" {
		t.Errorf("expected capz-e2e-abc123-ha, got %s", result.ClusterName)
	}
}

func TestMapTestToClusterHeuristics(t *testing.T) {
	clusters := []models.ClusterArtifacts{
		{ClusterName: "capz-e2e-abc123-ha"},
		{ClusterName: "capz-e2e-abc123-ipv6"},
		{ClusterName: "capz-e2e-abc123-windows"},
		{ClusterName: "capz-e2e-abc123-machine-pool"},
		{ClusterName: "capz-e2e-abc123-aks"},
		{ClusterName: "capz-e2e-abc123-azl3"},
		{ClusterName: "capz-e2e-abc123-dual-stack"},
		{ClusterName: "capz-e2e-abc123-flatcar-sysext"},
	}

	tests := []struct {
		testName string
		want     string
	}{
		{"[It] Creating a HA cluster", "capz-e2e-abc123-ha"},
		{"[It] IPv6 networking works", "capz-e2e-abc123-ipv6"},
		{"[It] Windows nodes join cluster", "capz-e2e-abc123-windows"},
		{"[It] VMSS scale set works", "capz-e2e-abc123-machine-pool"},
		{"[It] Machine pool test", "capz-e2e-abc123-machine-pool"},
		{"[It] AKS managed cluster", "capz-e2e-abc123-aks"},
		{"[It] Azure Linux 3 node pools", "capz-e2e-abc123-azl3"},
		{"[It] AZL3 distribution test", "capz-e2e-abc123-azl3"},
		{"[It] Dual-stack networking", "capz-e2e-abc123-dual-stack"},
		{"[It] DualStack test", "capz-e2e-abc123-dual-stack"},
		{"[It] Flatcar sysext cluster", "capz-e2e-abc123-flatcar-sysext"},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			result := MapTestToCluster(tt.testName, clusters)
			if result == nil {
				t.Fatalf("expected match for %q", tt.testName)
			}
			if result.ClusterName != tt.want {
				t.Errorf("MapTestToCluster(%q) = %s, want %s", tt.testName, result.ClusterName, tt.want)
			}
		})
	}
}

func TestMapTestToClusterNoMatch(t *testing.T) {
	clusters := []models.ClusterArtifacts{
		{ClusterName: "capz-e2e-abc123-ha"},
		{ClusterName: "capz-e2e-abc123-ipv6"},
	}

	result := MapTestToCluster("some completely unrelated test name", clusters)
	if result != nil {
		t.Errorf("expected nil for unmatched test, got %s", result.ClusterName)
	}
}

func TestMapTestToClusterEmpty(t *testing.T) {
	result := MapTestToCluster("any test", nil)
	if result != nil {
		t.Error("expected nil for empty clusters")
	}
}

func TestDiscoverClustersHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := discoverClustersFromURL(context.Background(), srv.Client(), srv.URL+"/", srv.URL+"/gcs/")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}
