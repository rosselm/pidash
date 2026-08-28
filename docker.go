// Docker container state, read straight off the engine's unix socket.
//
// The daemon speaks HTTP over /var/run/docker.sock, so net/http gets us there
// with nothing but a custom dialer — no docker client library required.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Container struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Image   string  `json:"image"`
	State   string  `json:"state"`
	Status  string  `json:"status"`
	Health  string  `json:"health"`
	CPU     float64 `json:"cpu"`
	MemUsed uint64  `json:"memUsed"`
	MemPct  float64 `json:"memPct"`
	Rx      uint64  `json:"rx"`
	Tx      uint64  `json:"tx"`
	Created int64   `json:"created"`
}

// maxStatsInFlight bounds concurrent /stats requests to the engine.
const maxStatsInFlight = 4

type dockerClient struct {
	http *http.Client
	sock string
}

func newDockerClient(sock string) *dockerClient {
	return &dockerClient{
		sock: sock,
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
		},
	}
}

func (c *dockerClient) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("docker %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type dockerListItem struct {
	ID      string   `json:"Id"`
	Names   []string `json:"Names"`
	Image   string   `json:"Image"`
	State   string   `json:"State"`
	Status  string   `json:"Status"`
	Created int64    `json:"Created"`
}

type dockerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PerCPUUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

// list returns every container, running or not, with per-container stats
// filled in for the running ones. Stats are fetched concurrently because the
// engine holds each stats request open for a sampling interval.
func (c *dockerClient) list(ctx context.Context) ([]Container, error) {
	var items []dockerListItem
	if err := c.get(ctx, "/containers/json?all=1", &items); err != nil {
		return nil, err
	}

	out := make([]Container, len(items))
	var wg sync.WaitGroup
	// The engine holds each stats request open for a full sampling interval, so
	// bound how many are in flight rather than opening one per container.
	sem := make(chan struct{}, maxStatsInFlight)
	for i, it := range items {
		name := strings.TrimPrefix(firstOr(it.Names, it.ID[:12]), "/")
		out[i] = Container{
			ID:      it.ID[:12],
			Name:    name,
			Image:   it.Image,
			State:   it.State,
			Status:  it.Status,
			Health:  healthFromStatus(it.Status),
			Created: it.Created,
		}
		if it.State != "running" {
			continue
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var st dockerStats
			if err := c.get(ctx, "/containers/"+id+"/stats?stream=false", &st); err != nil {
				return
			}
			out[i].CPU = statsCPUPercent(st)
			out[i].MemUsed, out[i].MemPct = statsMemory(st)
			if out[i].MemUsed == 0 {
				out[i].MemUsed = containerRSS(id)
			}
			for _, n := range st.Networks {
				out[i].Rx += n.RxBytes
				out[i].Tx += n.TxBytes
			}
		}(i, it.ID)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool {
		if (out[i].State == "running") != (out[j].State == "running") {
			return out[i].State == "running"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func statsCPUPercent(st dockerStats) float64 {
	cpuDelta := float64(st.CPUStats.CPUUsage.TotalUsage) - float64(st.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(st.CPUStats.SystemUsage) - float64(st.PreCPUStats.SystemUsage)
	if cpuDelta <= 0 || sysDelta <= 0 {
		return 0
	}
	n := st.CPUStats.OnlineCPUs
	if n == 0 {
		n = len(st.CPUStats.CPUUsage.PerCPUUsage)
	}
	if n == 0 {
		n = 1
	}
	return cpuDelta / sysDelta * float64(n) * 100
}

// statsMemory subtracts page cache the way `docker stats` does, so the number
// matches what the CLI would print.
func statsMemory(st dockerStats) (uint64, float64) {
	used := st.MemoryStats.Usage
	if cache, ok := st.MemoryStats.Stats["inactive_file"]; ok && cache < used {
		used -= cache
	} else if cache, ok := st.MemoryStats.Stats["total_inactive_file"]; ok && cache < used {
		used -= cache
	}
	var pct float64
	if st.MemoryStats.Limit > 0 {
		pct = 100 * float64(used) / float64(st.MemoryStats.Limit)
	}
	return used, pct
}

// containerRSS is the fallback for a host booted without the memory cgroup
// controller, where the engine reports no usage at all. Both cgroup drivers
// are checked because the path differs between systemd and cgroupfs.
func containerRSS(id string) uint64 {
	return cgroupRSS(firstExistingDir(
		"/sys/fs/cgroup/system.slice/docker-"+id+".scope",
		"/sys/fs/cgroup/docker/"+id,
	))
}

// healthFromStatus pulls "healthy" out of a status line like
// "Up 12 days (healthy)". Containers without a HEALTHCHECK have no parens.
func healthFromStatus(status string) string {
	_, rest, ok := strings.Cut(status, "(")
	if !ok {
		return ""
	}
	inner, _, ok := strings.Cut(rest, ")")
	if !ok || !strings.Contains(inner, "health") {
		return ""
	}
	return inner
}

func firstOr(s []string, fallback string) string {
	if len(s) > 0 {
		return s[0]
	}
	return fallback
}

// dockerUnavailable reports whether the failure is "no docker here" rather
// than a transient error, so the UI can hide the panel instead of alarming.
func dockerUnavailable(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
}
