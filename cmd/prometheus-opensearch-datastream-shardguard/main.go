package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	aws "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

type Exporter struct {
	client               *http.Client
	baseURL              string
	username             string
	password             string
	targetShardSizeBytes float64
	clusterName          string

	indexShardSizeDesc    *prometheus.Desc
	indexPrimaryStoreDesc *prometheus.Desc
	indexPrimaryCountDesc *prometheus.Desc
	indexRecommendedDesc  *prometheus.Desc
	indexShardOKDesc      *prometheus.Desc

	useIAM           bool
	awsSigner        *v4.Signer
	awsRegion        string
	awsService       string
	awsCredsProvider aws.CredentialsProvider
}

func NewExporter(
	baseURL, username, password string,
	targetShardSizeBytes float64,
	useIAM bool,
	awsRegion, awsService string,
) *Exporter {
	labelNames := []string{"cluster", "data_stream", "index"}

	exp := &Exporter{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		baseURL:              baseURL,
		username:             username,
		password:             password,
		targetShardSizeBytes: targetShardSizeBytes,
		useIAM:               useIAM,
		awsRegion:            awsRegion,
		awsService:           awsService,
		indexShardSizeDesc: prometheus.NewDesc(
			"opensearch_datastream_primary_shard_size_bytes",
			"Average size of primary shards for the latest backing index of a data stream, in bytes",
			labelNames,
			nil,
		),
		indexPrimaryStoreDesc: prometheus.NewDesc(
			"opensearch_datastream_primary_store_size_bytes",
			"Total primary store size (bytes) for the latest backing index of a data stream",
			labelNames,
			nil,
		),
		indexPrimaryCountDesc: prometheus.NewDesc(
			"opensearch_datastream_primary_shards",
			"Number of primary shards for the latest backing index of a data stream",
			labelNames,
			nil,
		),
		indexRecommendedDesc: prometheus.NewDesc(
			"opensearch_datastream_recommended_primary_shards",
			"Recommended number of primary shards for the latest backing index of a data stream based on target shard size",
			labelNames,
			nil,
		),
		indexShardOKDesc: prometheus.NewDesc(
			"opensearch_datastream_shard_size_ok",
			"1 if avg primary shard size for latest backing index is less than or equal to target, 0 otherwise",
			labelNames,
			nil,
		),
	}

	if useIAM {
		// Region can come from env or explicit arg; fallback to AWS_REGION
		if awsRegion == "" {
			awsRegion = os.Getenv("AWS_REGION")
		}
		exp.awsRegion = awsRegion
		if awsService == "" {
			awsService = "es"
		}
		exp.awsService = awsService

		cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(awsRegion),
		)
		if err != nil {
			log.Fatalf("failed to load AWS config for IAM auth: %v", err)
		}

		exp.awsSigner = v4.NewSigner()
		exp.awsCredsProvider = cfg.Credentials
	}

	return exp
}

// ------------ API response structs ------------

// cluster health
type clusterHealthResponse struct {
	ClusterName string `json:"cluster_name"`
}

// _stats/store
type statsStoreResponse struct {
	Indices map[string]struct {
		Primaries struct {
			Store struct {
				SizeInBytes float64 `json:"size_in_bytes"`
			} `json:"store"`
		} `json:"primaries"`
	} `json:"indices"`
}

// _cat/indices?format=json&h=index,pri,rep
type catIndexEntry struct {
	Index string `json:"index"`
	Pri   string `json:"pri"`
	Rep   string `json:"rep"`
}

// _data_stream
type dataStreamsResponse struct {
	DataStreams []struct {
		Name    string `json:"name"`
		Indices []struct {
			IndexName string `json:"index_name"`
		} `json:"indices"`
	} `json:"data_streams"`
}

// ------------ Prometheus interfaces ------------

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.indexShardSizeDesc
	ch <- e.indexPrimaryStoreDesc
	ch <- e.indexPrimaryCountDesc
	ch <- e.indexRecommendedDesc
	ch <- e.indexShardOKDesc
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	// 1) Ensure cluster name is cached
	if e.clusterName == "" {
		if err := e.fetchClusterName(); err != nil {
			log.Printf("error fetching cluster name: %v", err)
			// continue; we can still emit metrics, cluster label will be empty
		}
	}
	cluster := e.clusterName

	// 2) Fetch data streams (to know which backing indices to care about)
	dataStreamIndexMap, err := e.fetchDataStreamLatestIndices()
	if err != nil {
		log.Printf("error fetching data streams: %v", err)
		return
	}

	// build slice of backing index names we care about
	backingIndices := make([]string, 0, len(dataStreamIndexMap))
	for indexName := range dataStreamIndexMap {
		backingIndices = append(backingIndices, indexName)
	}

	if len(backingIndices) == 0 {
		// nothing to do
		return
	}

	// 3) Fetch stats + cat indices
	stats, err := e.fetchStatsStoreForIndices(backingIndices)
	if err != nil {
		log.Printf("error fetching stats store: %v", err)
		return
	}
	catIndices, err := e.fetchCatIndicesForIndices(backingIndices)
	if err != nil {
		log.Printf("error fetching cat indices: %v", err)
		return
	}

	// build: index -> primary shard count (only for backing indices)
	primaries := map[string]int{}
	for _, entry := range catIndices {
		priCount, err := strconv.Atoi(entry.Pri)
		if err != nil {
			log.Printf("error parsing primary shard count for index %s: %v", entry.Index, err)
			continue
		}
		primaries[entry.Index] = priCount
	}

	// For each backing index (latest per data stream) compute metrics
	for indexName, dataStreamName := range dataStreamIndexMap {
		idxStats, ok := stats.Indices[indexName]
		if !ok {
			// stats missing (index disappeared?), skip
			continue
		}

		primaryStoreBytes := idxStats.Primaries.Store.SizeInBytes
		primaryCount, ok := primaries[indexName]
		if !ok || primaryCount <= 0 {
			continue
		}

		avgSize := primaryStoreBytes / float64(primaryCount)
		recommended := math.Ceil(primaryStoreBytes / e.targetShardSizeBytes)

		shardOK := 1.0
		if avgSize > e.targetShardSizeBytes {
			shardOK = 0.0
		}

		labels := []string{cluster, dataStreamName, indexName}

		ch <- prometheus.MustNewConstMetric(
			e.indexPrimaryStoreDesc,
			prometheus.GaugeValue,
			primaryStoreBytes,
			labels...,
		)
		ch <- prometheus.MustNewConstMetric(
			e.indexPrimaryCountDesc,
			prometheus.GaugeValue,
			float64(primaryCount),
			labels...,
		)
		ch <- prometheus.MustNewConstMetric(
			e.indexShardSizeDesc,
			prometheus.GaugeValue,
			avgSize,
			labels...,
		)
		ch <- prometheus.MustNewConstMetric(
			e.indexRecommendedDesc,
			prometheus.GaugeValue,
			recommended,
			labels...,
		)
		ch <- prometheus.MustNewConstMetric(
			e.indexShardOKDesc,
			prometheus.GaugeValue,
			shardOK,
			labels...,
		)
	}

}

// ------------ HTTP helpers ------------

func (e *Exporter) doRequest(method, path string) (*http.Response, error) {
	req, err := http.NewRequest(method, e.baseURL+path, nil)
	if err != nil {
		return nil, err
	}

	// If IAM is enabled, sign with SigV4.
	if e.useIAM {
		if e.awsSigner == nil || e.awsCredsProvider == nil {
			return nil, fmt.Errorf("IAM auth enabled but signer or credentials provider is not initialized")
		}

		creds, err := e.awsCredsProvider.Retrieve(context.Background())
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve AWS credentials: %w", err)
		}

		// No body, so use the special unsigned payload hash.
		const unsignedPayloadHash = "UNSIGNED-PAYLOAD"

		err = e.awsSigner.SignHTTP(
			context.Background(),
			creds,
			req,
			unsignedPayloadHash,
			e.awsService,
			e.awsRegion,
			time.Now(),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign request with SigV4: %w", err)
		}
	} else if e.username != "" {
		// Fallback: basic auth.
		req.SetBasicAuth(e.username, e.password)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d for %s %s", resp.StatusCode, method, path)
	}
	return resp, nil
}

// ------------ Fetchers ------------

func (e *Exporter) fetchClusterName() error {
	resp, err := e.doRequest("GET", "/_cluster/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var chRes clusterHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&chRes); err != nil {
		return err
	}
	e.clusterName = chRes.ClusterName
	return nil
}

func (e *Exporter) fetchStatsStoreForIndices(indices []string) (*statsStoreResponse, error) {
	if len(indices) == 0 {
		return &statsStoreResponse{Indices: map[string]struct {
			Primaries struct {
				Store struct {
					SizeInBytes float64 `json:"size_in_bytes"`
				} `json:"store"`
			} `json:"primaries"`
		}{}}, nil
	}

	path := "/" + strings.Join(indices, ",") + "/_stats/store"
	resp, err := e.doRequest("GET", path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var stats statsStoreResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

func (e *Exporter) fetchCatIndicesForIndices(indices []string) ([]catIndexEntry, error) {
	if len(indices) == 0 {
		return nil, nil
	}

	path := "/_cat/indices/" + strings.Join(indices, ",") + "?format=json&h=index,pri,rep"
	resp, err := e.doRequest("GET", path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var entries []catIndexEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// fetchDataStreamLatestIndices returns a map[backingIndexName]dataStreamName
// but only for the latest backing index in each data stream.
func (e *Exporter) fetchDataStreamLatestIndices() (map[string]string, error) {
	resp, err := e.doRequest("GET", "/_data_stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dsResp dataStreamsResponse
	if err := json.NewDecoder(resp.Body).Decode(&dsResp); err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, ds := range dsResp.DataStreams {
		if len(ds.Indices) == 0 {
			continue
		}
		// assume the last index is the latest backing index (write index)
		latest := ds.Indices[len(ds.Indices)-1]
		result[latest.IndexName] = ds.Name
	}
	return result, nil
}

// ------------ main ------------

func main() {
	baseURL := os.Getenv("OPENSEARCH_URL")
	if baseURL == "" {
		log.Fatal("OPENSEARCH_URL is required")
	}
	username := os.Getenv("OPENSEARCH_USERNAME")
	password := os.Getenv("OPENSEARCH_PASSWORD")

	targetGBStr := os.Getenv("TARGET_SHARD_SIZE_GB")
	if targetGBStr == "" {
		targetGBStr = "30"
	}
	targetGB, err := strconv.ParseFloat(targetGBStr, 64)
	if err != nil {
		log.Fatalf("invalid TARGET_SHARD_SIZE_GB: %v", err)
	}
	targetBytes := targetGB * 1024 * 1024 * 1024

	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":9108"
	}

	// IAM-related envs
	useIAM := false
	if v := os.Getenv("OPENSEARCH_USE_IAM"); v == "true" || v == "1" {
		useIAM = true
	}
	awsRegion := os.Getenv("OPENSEARCH_AWS_REGION")
	awsService := os.Getenv("OPENSEARCH_AWS_SERVICE")

	exporter := NewExporter(baseURL, username, password, targetBytes, useIAM, awsRegion, awsService)
	prometheus.MustRegister(exporter)

	http.Handle("/metrics", promhttp.Handler())

	authMode := "no-auth"
	if useIAM {
		authMode = "iam"
	} else if username != "" {
		authMode = "basic"
	}

	log.Printf(
		"Starting OpenSearch data stream shard exporter on %s, target shard %.1f GB, auth=%s",
		listenAddr, targetGB, authMode,
	)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("error starting HTTP server: %v", err)
	}
}
