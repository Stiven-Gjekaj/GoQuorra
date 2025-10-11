package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/spf13/cobra"
)

var (
	serverURL string
	apiKey    string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "quorractl",
		Short: "GoQuorra CLI - Manage jobs and queues",
		Long:  "Command-line tool to interact with GoQuorra job queue system",
	}

	rootCmd.PersistentFlags().StringVar(&serverURL, "server", "http://localhost:8080", "GoQuorra server URL")
	rootCmd.PersistentFlags().StringVar(&apiKey, "api-key", "dev-api-key-change-in-production", "API key for authentication")

	// Create job command
	createCmd := &cobra.Command{
		Use:   "create TYPE",
		Short: "Create a new job",
		Long:  "Create a new job with specified type and payload",
		Args:  cobra.ExactArgs(1),
		Run:   createJob,
	}
	createCmd.Flags().String("payload", "{}", "Job payload as JSON string")
	createCmd.Flags().String("queue", "default", "Queue name")
	createCmd.Flags().Int("priority", 0, "Job priority")
	createCmd.Flags().Int("delay", 0, "Delay in seconds before job is ready")
	createCmd.Flags().Int("retries", 3, "Maximum number of retries")

	// Get job command
	getCmd := &cobra.Command{
		Use:   "get JOB_ID",
		Short: "Get job details",
		Args:  cobra.ExactArgs(1),
		Run:   getJob,
	}

	// List queues command
	queuesCmd := &cobra.Command{
		Use:   "queues",
		Short: "List queue statistics",
		Run:   listQueues,
	}

	// Stats command (alias for queues)
	statsCmd := &cobra.Command{
		Use:   "stats",
		Short: "Show queue statistics",
		Run:   listQueues,
	}

	rootCmd.AddCommand(createCmd, getCmd, queuesCmd, statsCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func createJob(cmd *cobra.Command, args []string) {
	jobType := args[0]
	payloadStr, _ := cmd.Flags().GetString("payload")
	queue, _ := cmd.Flags().GetString("queue")
	priority, _ := cmd.Flags().GetInt("priority")
	delay, _ := cmd.Flags().GetInt("delay")
	retries, _ := cmd.Flags().GetInt("retries")

	// Parse payload
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Invalid JSON payload: %v\n", err)
		os.Exit(1)
	}

	// Create request
	reqBody := map[string]interface{}{
		"type":          jobType,
		"payload":       payload,
		"queue":         queue,
		"priority":      priority,
		"delay_seconds": delay,
		"max_retries":   retries,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to marshal request: %v\n", err)
		os.Exit(1)
	}

	// Send request
	req, err := http.NewRequest("POST", serverURL+"/v1/jobs", bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to create request: %v\n", err)
		os.Exit(1)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to send request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to read response: %v\n", err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusCreated {
		fmt.Fprintf(os.Stderr, "Error: Server returned status %d\n%s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to parse response: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Job created successfully!\n")
	fmt.Printf("ID:     %s\n", result["id"])
	fmt.Printf("Status: %s\n", result["status"])
	fmt.Printf("Run at: %s\n", result["run_at"])
}

func getJob(cmd *cobra.Command, args []string) {
	jobID := args[0]

	req, err := http.NewRequest("GET", serverURL+"/v1/jobs/"+jobID, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to create request: %v\n", err)
		os.Exit(1)
	}

	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to send request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to read response: %v\n", err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: Server returned status %d\n%s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	var job map[string]interface{}
	if err := json.Unmarshal(body, &job); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to parse response: %v\n", err)
		os.Exit(1)
	}

	// Pretty print
	prettyJSON, _ := json.MarshalIndent(job, "", "  ")
	fmt.Println(string(prettyJSON))
}

func listQueues(cmd *cobra.Command, args []string) {
	req, err := http.NewRequest("GET", serverURL+"/v1/queues", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to create request: %v\n", err)
		os.Exit(1)
	}

	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to send request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to read response: %v\n", err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: Server returned status %d\n%s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	var result struct {
		Queues []struct {
			Queue  string `json:"queue"`
			Status string `json:"status"`
			Count  int    `json:"count"`
		} `json:"queues"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to parse response: %v\n", err)
		os.Exit(1)
	}

	// Group by queue
	queueStats := make(map[string]map[string]int)
	for _, stat := range result.Queues {
		if _, exists := queueStats[stat.Queue]; !exists {
			queueStats[stat.Queue] = make(map[string]int)
		}
		queueStats[stat.Queue][stat.Status] = stat.Count
	}

	fmt.Println("Queue Statistics:")
	fmt.Println("─────────────────────────────────────────")
	for queue, stats := range queueStats {
		fmt.Printf("\n%s:\n", queue)
		for status, count := range stats {
			fmt.Printf("  %-12s: %d\n", status, count)
		}
	}
}
