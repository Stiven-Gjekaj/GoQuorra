package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/goquorra/goquorra/internal/metrics"
	"github.com/goquorra/goquorra/internal/queue"
	"github.com/goquorra/goquorra/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler handles HTTP API requests
type Handler struct {
	queueManager *queue.Manager
	store        store.Store
	metrics      *metrics.Collector
	apiKey       string
	logger       *log.Logger
}

// NewHandler creates a new API handler
func NewHandler(store store.Store, queueManager *queue.Manager, metrics *metrics.Collector, apiKey string, logger *log.Logger) *Handler {
	return &Handler{
		queueManager: queueManager,
		store:        store,
		metrics:      metrics,
		apiKey:       apiKey,
		logger:       logger,
	}
}

// Router creates and configures the HTTP router
func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Heartbeat("/healthz"))

	// Public routes
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	// Dashboard
	r.Get("/", h.serveDashboard)
	r.Get("/dashboard", h.serveDashboard)

	// API routes with authentication
	r.Route("/v1", func(r chi.Router) {
		r.Use(h.authMiddleware)

		// Job endpoints
		r.Post("/jobs", h.createJob)
		r.Get("/jobs/{id}", h.getJob)

		// Queue endpoints
		r.Get("/queues", h.getQueues)

		// Recent jobs for dashboard
		r.Get("/recent", h.getRecentJobs)
	})

	return r
}

// authMiddleware validates API key
func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			apiKey = r.URL.Query().Get("api_key")
		}

		if apiKey != h.apiKey {
			h.respondError(w, http.StatusUnauthorized, "Invalid or missing API key")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// createJob handles POST /v1/jobs
func (h *Handler) createJob(w http.ResponseWriter, r *http.Request) {
	var req store.CreateJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Validation
	if req.Type == "" {
		h.respondError(w, http.StatusBadRequest, "Job type is required")
		return
	}
	if req.Payload == nil {
		req.Payload = make(map[string]interface{})
	}
	if req.Queue == "" {
		req.Queue = "default"
	}
	if req.MaxRetries == 0 {
		req.MaxRetries = 3
	}

	job, err := h.queueManager.EnqueueJob(r.Context(), &req)
	if err != nil {
		h.logger.Printf("Failed to create job: %v", err)
		h.respondError(w, http.StatusInternalServerError, "Failed to create job")
		return
	}

	h.metrics.JobsCreated.Inc()

	h.respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":     job.ID,
		"status": job.Status,
		"run_at": job.RunAt,
	})
}

// getJob handles GET /v1/jobs/{id}
func (h *Handler) getJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		h.respondError(w, http.StatusBadRequest, "Job ID is required")
		return
	}

	job, err := h.queueManager.GetJob(r.Context(), id)
	if err != nil {
		h.respondError(w, http.StatusNotFound, "Job not found")
		return
	}

	h.respondJSON(w, http.StatusOK, job)
}

// getQueues handles GET /v1/queues
func (h *Handler) getQueues(w http.ResponseWriter, r *http.Request) {
	stats, err := h.queueManager.GetQueueStats(r.Context())
	if err != nil {
		h.logger.Printf("Failed to get queue stats: %v", err)
		h.respondError(w, http.StatusInternalServerError, "Failed to get queue stats")
		return
	}

	h.respondJSON(w, http.StatusOK, map[string]interface{}{
		"queues": stats,
	})
}

// getRecentJobs handles GET /v1/recent
func (h *Handler) getRecentJobs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 1000 {
			limit = l
		}
	}

	jobs, err := h.queueManager.GetRecentJobs(r.Context(), limit)
	if err != nil {
		h.logger.Printf("Failed to get recent jobs: %v", err)
		h.respondError(w, http.StatusInternalServerError, "Failed to get recent jobs")
		return
	}

	h.respondJSON(w, http.StatusOK, map[string]interface{}{
		"jobs": jobs,
	})
}

// serveDashboard serves the web dashboard
func (h *Handler) serveDashboard(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>GoQuorra Dashboard</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #f5f5f5; color: #333; }
        header { background: #2c3e50; color: white; padding: 1.5rem; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
        h1 { font-size: 1.8rem; }
        .subtitle { color: #bdc3c7; font-size: 0.9rem; margin-top: 0.25rem; }
        .container { max-width: 1200px; margin: 2rem auto; padding: 0 1rem; }
        .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(250px, 1fr)); gap: 1rem; margin-bottom: 2rem; }
        .card { background: white; padding: 1.5rem; border-radius: 8px; box-shadow: 0 1px 3px rgba(0,0,0,0.1); }
        .card h2 { font-size: 1rem; color: #7f8c8d; margin-bottom: 0.5rem; text-transform: uppercase; font-weight: 600; }
        .card .value { font-size: 2rem; font-weight: bold; color: #2c3e50; }
        .status-pending { color: #f39c12; }
        .status-leased { color: #3498db; }
        .status-succeeded { color: #27ae60; }
        .status-failed { color: #e74c3c; }
        .status-dead { color: #c0392b; }
        table { width: 100%; background: white; border-radius: 8px; overflow: hidden; box-shadow: 0 1px 3px rgba(0,0,0,0.1); }
        th, td { padding: 1rem; text-align: left; border-bottom: 1px solid #ecf0f1; }
        th { background: #34495e; color: white; font-weight: 600; }
        tr:last-child td { border-bottom: none; }
        .badge { display: inline-block; padding: 0.25rem 0.75rem; border-radius: 12px; font-size: 0.75rem; font-weight: 600; text-transform: uppercase; }
        .badge-pending { background: #fff3cd; color: #856404; }
        .badge-leased { background: #cce5ff; color: #004085; }
        .badge-succeeded { background: #d4edda; color: #155724; }
        .badge-failed { background: #f8d7da; color: #721c24; }
        .badge-dead { background: #f5c6cb; color: #491217; }
        .code { font-family: 'Courier New', monospace; background: #ecf0f1; padding: 0.25rem 0.5rem; border-radius: 3px; font-size: 0.85rem; }
        .refresh { float: right; background: #3498db; color: white; border: none; padding: 0.5rem 1rem; border-radius: 4px; cursor: pointer; }
        .refresh:hover { background: #2980b9; }
    </style>
</head>
<body>
    <header>
        <h1>GoQuorra</h1>
        <div class="subtitle">Distributed Job Queue Dashboard</div>
    </header>
    <div class="container">
        <button class="refresh" onclick="loadData()">Refresh</button>
        <h2 style="margin-bottom: 1rem; color: #2c3e50;">Queue Statistics</h2>
        <div class="grid" id="stats"></div>

        <h2 style="margin: 2rem 0 1rem; color: #2c3e50;">Recent Jobs</h2>
        <table id="jobs">
            <thead>
                <tr>
                    <th>ID</th>
                    <th>Type</th>
                    <th>Queue</th>
                    <th>Status</th>
                    <th>Priority</th>
                    <th>Attempts</th>
                    <th>Created</th>
                </tr>
            </thead>
            <tbody></tbody>
        </table>
    </div>
    <script>
        async function loadData() {
            try {
                const [queuesRes, jobsRes] = await Promise.all([
                    fetch('/v1/queues?api_key=dev-api-key-change-in-production'),
                    fetch('/v1/recent?limit=20&api_key=dev-api-key-change-in-production')
                ]);

                const queues = await queuesRes.json();
                const jobs = await jobsRes.json();

                renderStats(queues.queues || []);
                renderJobs(jobs.jobs || []);
            } catch (err) {
                console.error('Failed to load data:', err);
            }
        }

        function renderStats(stats) {
            const grouped = {};
            stats.forEach(s => {
                if (!grouped[s.queue]) grouped[s.queue] = {};
                grouped[s.queue][s.status] = s.count;
            });

            let html = '';
            for (const [queue, counts] of Object.entries(grouped)) {
                html += '<div class="card">';
                html += '<h2>' + queue + '</h2>';
                html += '<div style="margin-top: 0.5rem; font-size: 0.9rem;">';
                for (const [status, count] of Object.entries(counts)) {
                    html += '<div style="margin: 0.25rem 0;">';
                    html += '<span class="status-' + status + '">' + status + '</span>: ' + count;
                    html += '</div>';
                }
                html += '</div></div>';
            }

            document.getElementById('stats').innerHTML = html || '<div class="card">No queue data available</div>';
        }

        function renderJobs(jobs) {
            const tbody = document.querySelector('#jobs tbody');
            if (!jobs || jobs.length === 0) {
                tbody.innerHTML = '<tr><td colspan="7" style="text-align:center; color:#7f8c8d;">No jobs found</td></tr>';
                return;
            }

            tbody.innerHTML = jobs.map(job => {
                const created = new Date(job.created_at).toLocaleString();
                return '<tr>' +
                    '<td><span class="code">' + job.id.substring(0, 8) + '</span></td>' +
                    '<td>' + job.type + '</td>' +
                    '<td>' + job.queue + '</td>' +
                    '<td><span class="badge badge-' + job.status + '">' + job.status + '</span></td>' +
                    '<td>' + job.priority + '</td>' +
                    '<td>' + job.attempts + '/' + job.max_retries + '</td>' +
                    '<td>' + created + '</td>' +
                    '</tr>';
            }).join('');
        }

        loadData();
        setInterval(loadData, 5000);
    </script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// respondJSON sends a JSON response
func (h *Handler) respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// respondError sends an error response
func (h *Handler) respondError(w http.ResponseWriter, status int, message string) {
	h.respondJSON(w, status, map[string]string{"error": message})
}
