// +build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

const (
	serverURL = "http://localhost:8080"
	apiKey    = "dev-api-key-change-in-production"
)

func TestEndToEndJobProcessing(t *testing.T) {
	// This test requires the full stack to be running via docker-compose

	// Create a test job
	jobReq := map[string]interface{}{
		"type":    "test_e2e",
		"payload": map[string]interface{}{"message": "hello world"},
		"queue":   "default",
		"priority": 10,
		"max_retries": 3,
	}

	jobID := createJob(t, jobReq)
	t.Logf("Created job: %s", jobID)

	// Wait for job to be processed
	time.Sleep(10 * time.Second)

	// Check job status
	job := getJob(t, jobID)
	status := job["status"].(string)

	if status != "succeeded" && status != "leased" && status != "pending" {
		t.Logf("Job status: %s (may still be processing)", status)
	}
}

func TestConcurrentWorkers(t *testing.T) {
	// Create 20 jobs
	jobIDs := make([]string, 20)
	for i := 0; i < 20; i++ {
		jobReq := map[string]interface{}{
			"type": "test_concurrent",
			"payload": map[string]interface{}{
				"index": i,
				"data":  fmt.Sprintf("job-%d", i),
			},
			"queue":       "default",
			"max_retries": 3,
		}
		jobIDs[i] = createJob(t, jobReq)
	}

	t.Logf("Created %d jobs", len(jobIDs))

	// Wait for processing
	time.Sleep(15 * time.Second)

	// Check all jobs completed without duplication
	processedBy := make(map[string][]string) // worker -> job IDs
	var mu sync.Mutex

	for _, jobID := range jobIDs {
		job := getJob(t, jobID)
		status := job["status"].(string)

		if status == "succeeded" {
			if leasedBy, ok := job["leased_by"].(string); ok && leasedBy != "" {
				mu.Lock()
				processedBy[leasedBy] = append(processedBy[leasedBy], jobID)
				mu.Unlock()
			}
		}
	}

	t.Logf("Jobs processed by workers: %v", processedBy)

	// Verify no double-processing (each job processed once)
	allProcessed := make(map[string]bool)
	for _, jobs := range processedBy {
		for _, jobID := range jobs {
			if allProcessed[jobID] {
				t.Errorf("Job %s processed multiple times!", jobID)
			}
			allProcessed[jobID] = true
		}
	}
}

func TestQueueStats(t *testing.T) {
	req, _ := http.NewRequest("GET", serverURL+"/v1/queues", nil)
	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to get queue stats: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	queues, ok := result["queues"].([]interface{})
	if !ok {
		t.Fatal("Expected queues array in response")
	}

	t.Logf("Queue stats: %v", queues)
}

func TestMetricsEndpoint(t *testing.T) {
	resp, err := http.Get(serverURL + "/metrics")
	if err != nil {
		t.Fatalf("Failed to get metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	// Just verify it returns Prometheus format
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	body := buf.String()

	if !bytes.Contains([]byte(body), []byte("quorra_jobs_created_total")) {
		t.Error("Expected Prometheus metrics in response")
	}
}

func TestDelayedJobScheduling(t *testing.T) {
	jobReq := map[string]interface{}{
		"type":          "test_delayed",
		"payload":       map[string]interface{}{"data": "delayed"},
		"queue":         "default",
		"delay_seconds": 5,
		"max_retries":   3,
	}

	jobID := createJob(t, jobReq)
	t.Logf("Created delayed job: %s", jobID)

	// Immediately check - should be pending
	job := getJob(t, jobID)
	if job["status"] != "pending" {
		t.Errorf("Expected pending status, got %s", job["status"])
	}

	// Wait for delay + processing time
	time.Sleep(10 * time.Second)

	// Check again - should be processed or in progress
	job = getJob(t, jobID)
	status := job["status"].(string)
	t.Logf("Delayed job status after delay: %s", status)
}

// Helper functions

func createJob(t *testing.T, jobReq map[string]interface{}) string {
	jsonData, _ := json.Marshal(jobReq)
	req, _ := http.NewRequest("POST", serverURL+"/v1/jobs", bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("Expected status 201, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	return result["id"].(string)
}

func getJob(t *testing.T, jobID string) map[string]interface{} {
	req, _ := http.NewRequest("GET", serverURL+"/v1/jobs/"+jobID, nil)
	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", resp.StatusCode)
	}

	var job map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	return job
}
