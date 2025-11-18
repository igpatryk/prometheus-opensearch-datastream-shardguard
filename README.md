# Prometheus OpenSearch Data Stream Shard Guard

`prometheus-opensearch-datastream-shardguard` is a Prometheus exporter that inspects **OpenSearch data streams** and checks whether the **latest backing index** (write index) has a reasonable shard sizing.

It calculates:

- Average primary shard size for each data stream’s latest backing index
- Recommended primary shard count (based on a target shard size, default **30 GB**)
- A simple “OK / not OK” flag you can alert on

Perfect for clusters where you want to keep shards around a sane size and avoid **monster** shards.

---

## Features

- Focused on **data streams** (ignores legacy indices)
- Only inspects the **latest backing index** of each data stream
- Exposes Prometheus metrics labeled by:
  - `cluster`
  - `data_stream`
  - `index` (backing index name)
- Configurable **target shard size** (in GB)
- Simple, single-binary exporter written in Go

---

## How it works

On each scrape, the exporter:

1. Fetches cluster name:
   - `GET /_cluster/health`
2. Fetches data streams and picks the latest backing index for each:
   - `GET /_data_stream`
3. Fetches store stats for indices:
   - `GET /_stats/store`
4. Fetches shard counts per index:
   - `GET /_cat/indices?format=json&h=index,pri,rep`
5. For each **latest backing index**:
   - Calculates total primary store size
   - Divides by primary shard count → **average primary shard size**
   - Computes a recommended primary shard count using the configured target shard size
   - Checks if the average primary shard size is **above the target shard size** and sets an `ok` flag:
     - `shard_size_ok = 1` if avg shard size ≤ target
     - `shard_size_ok = 0` if avg shard size > target

## Testing locally

1. Spin up local OpenSearch
```bash
docker compose up -d
```
2. Create a data stream and index some docs
```bash
curl -X PUT "http://localhost:9200/_index_template/logs-template" \
  -H 'Content-Type: application/json' \
  -d '{
    "index_patterns": ["logs-*"],
    "data_stream": {},
    "template": {
      "mappings": {
        "properties": {
          "@timestamp": { "type": "date" },
          "message":    { "type": "text" },
          "level":      { "type": "keyword" }
        }
      }
    }
  }'

curl -X PUT "http://localhost:9200/_data_stream/logs-web"

curl -X POST "http://localhost:9200/logs-web/_bulk" \
  -H 'Content-Type: application/x-ndjson' \
  -d $'{"create":{}}\n{"@timestamp":"2025-11-18T00:00:00Z","message":"hello 1","level":"info"}\n{"create":{}}\n{"@timestamp":"2025-11-18T00:01:00Z","message":"hello 2","level":"error"}\n'
```
3. Run the exporter locally
```bash
docker build -t prometheus-opensearch-datastream-shardguard:dev .
docker run --rm -p 9108:9108 \
  -e OPENSEARCH_URL="http://host.docker.internal:9200" \
  -e TARGET_SHARD_SIZE_GB="1" \
  -e LISTEN_ADDR=":9108" \
  prometheus-opensearch-datastream-shardguard:dev
```
4. Check metrics
```bash
curl http://localhost:9108/metrics | grep opensearch_datastream
```
---

## Metrics

All metrics are labeled with:

- `cluster` – OpenSearch cluster name
- `data_stream` – data stream name (e.g. `logs-web`)
- `index` – latest backing index name (e.g. `.ds-logs-web-2025.11.19-000002`)

Exported metrics:

- `opensearch_datastream_primary_store_size_bytes`  
  Total **primary** store size for the latest backing index (bytes).

- `opensearch_datastream_primary_shards`  
  Number of **primary** shards for the latest backing index.

- `opensearch_datastream_primary_shard_size_bytes`  
  **Average** primary shard size (bytes) = `primary_store_size_bytes / primary_shards`.

- `opensearch_datastream_recommended_primary_shards`  
  Recommended number of primary shards based on the configured target shard size.

- `opensearch_datastream_shard_size_ok`  
  `1` if `avg_primary_shard_size` is **less than or equal to the configured target shard size**, otherwise `0`.

### Example PromQL

Indices with “bad” shard sizing:

```promql
opensearch_datastream_shard_size_ok == 0
```