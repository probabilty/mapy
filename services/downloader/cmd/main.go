package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"
)

// Source describes a single artefact (e.g. Egypt PBF dump) that we might need to download.
type Source struct {
	Name   string `yaml:"name"`   // friendly identifier ("egypt-latest")
	URL    string `yaml:"url"`    // remote HTTPS URL
	Path   string `yaml:"path"`   // relative file path under DATA_DIR (e.g. africa/egypt-latest.osm.pbf)
	SHA256 string `yaml:"sha256"` // optional expected checksum from upstream checksums.txt
}

// Completed is the event envelope we put on NATS once a download either completes or is confirmed present.
type Completed struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	Status     string    `json:"status"` // "downloaded" | "skipped"
	FinishedAt time.Time `json:"finished_at"`
}

func main() {
	cfgPath := getenv("OSM_SOURCES_FILE", "/config/osm_sources.yaml")
	dataDir := getenv("DATA_DIR", "/data")
	natsURL := getenv("NATS_URL", "nats://nats:4222")

	// 1. Load YAML config
	sources, err := loadSources(cfgPath)
	if err != nil {
		log.Fatalf("loadSources: %v", err)
	}
	log.Printf("found %d sources in %s", len(sources), cfgPath)

	// 2. Connect to NATS (JetStream optional but nice to have)
	nc, err := nats.Connect(natsURL, nats.Name("osm-downloader"))
	if err != nil {
		log.Fatalf("connect nats: %v", err)
	}
	defer nc.Drain()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for _, src := range sources {
		select {
		case <-ctx.Done():
			log.Println("shutdown requested – exiting loop")
			return
		default:
		}

		if err := handleSource(ctx, nc, dataDir, src); err != nil {
			log.Printf("[%s] ERROR: %v", src.Name, err)
		}
	}
}

// handleSource decides whether we need to download the file and publishes a Completed event accordingly.
func handleSource(ctx context.Context, nc *nats.Conn, dataDir string, s Source) error {
	dest := filepath.Join(dataDir, s.Path)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	status := "downloaded"
	var size int64
	var sum string

	// ----------- Skip logic ----------------
	if fileExists(dest) {
		if s.SHA256 != "" {
			localSum, err := fileSHA256(dest)
			if err == nil && localSum == s.SHA256 {
				status = "skipped"
				sum = localSum
				if fi, _ := os.Stat(dest); fi != nil {
					size = fi.Size()
				}
				log.Printf("[%s] up‑to‑date – skipping download", s.Name)
			} else {
				log.Printf("[%s] checksum mismatch – redownloading", s.Name)
			}
		} else {
			log.Printf("[%s] file exists (checksum unknown) – skipping", s.Name)
			status = "skipped"
			if fi, _ := os.Stat(dest); fi != nil {
				size = fi.Size()
			}
		}
	}

	// ----------- Download (if needed) ------
	if status == "downloaded" {
		n, sha, err := downloadFile(ctx, s.URL, dest)
		if err != nil {
			return fmt.Errorf("download: %w", err)
		}
		size = n
		sum = sha
		log.Printf("[%s] downloaded %d bytes", s.Name, n)
	}

	// ----------- Publish event -------------
	evt := Completed{
		Name:       s.Name,
		Path:       dest,
		SizeBytes:  size,
		SHA256:     sum,
		Status:     status,
		FinishedAt: time.Now().UTC(),
	}
	payload, _ := json.Marshal(evt)
	if err := nc.Publish("osm.download.completed", payload); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// ------------------------ UTILITIES ------------------------

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func loadSources(path string) ([]Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Source
	if err := yaml.NewDecoder(f).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func downloadFile(ctx context.Context, url, dest string) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("unexpected status %s", resp.Status)
	}

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return 0, "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(hash.Sum(nil)), nil
}
