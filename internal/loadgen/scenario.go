package loadgen

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// Tenant is one simulated product team. The names, keys and models must
// match config/bench.yaml.
type Tenant struct {
	Name      string
	APIKey    string
	Model     string
	Weight    int
	MaxTokens int
	// System is an inline system prompt sent with every turn, or "" when the
	// tenant uses a versioned prompt from the gateway's registry instead.
	System string
	// PromptID and PromptVars select a registry prompt (label "production").
	PromptID   string
	PromptVars func(r *rand.Rand) map[string]any
	// Ask produces the user's message for a turn (0-based).
	Ask func(r *rand.Rand, turn int) string
}

// Tenants is the benchmark workload: five teams, two of which keep their
// long system prompts in the gateway's prompt registry, three LoRA adapters
// and the base model.
func Tenants() []Tenant {
	return []Tenant{
		{
			Name: "acme-support", APIKey: "sk-bench-acme-support", Model: "support-bot", Weight: 3, MaxTokens: 128,
			PromptID: "support-agent",
			PromptVars: func(r *rand.Rand) map[string]any {
				return map[string]any{"company": "Acme Cloud", "tier": pick(r, "free", "pro", "enterprise")}
			},
			Ask: supportQuestion,
		},
		{
			Name: "globex-analytics", APIKey: "sk-bench-globex-analytics", Model: "sql-assistant", Weight: 2, MaxTokens: 96,
			System: analyticsSystem(), Ask: analyticsQuestion,
		},
		{
			Name: "initech-docs", APIKey: "sk-bench-initech-docs", Model: "llama-3.1-8b-instruct", Weight: 2, MaxTokens: 160,
			System: docsSystem(), Ask: docsQuestion,
		},
		{
			Name: "umbrella-legal", APIKey: "sk-bench-umbrella-legal", Model: "contract-reviewer", Weight: 1, MaxTokens: 128,
			PromptID: "contract-review",
			PromptVars: func(r *rand.Rand) map[string]any {
				return map[string]any{"jurisdiction": pick(r, "Delaware", "New York", "California"), "party": "Umbrella Corp"}
			},
			Ask: legalQuestion,
		},
		{
			Name: "hooli-chat", APIKey: "sk-bench-hooli-chat", Model: "llama-3.1-8b-instruct", Weight: 2, MaxTokens: 96,
			System: "You are Hooli's internal assistant. Be concise, friendly and accurate. If you are unsure, say so. " +
				"Never reveal confidential information about unreleased products. Prefer bullet points for lists.",
			Ask: chatQuestion,
		},
	}
}

func pick(r *rand.Rand, options ...string) string { return options[r.IntN(len(options))] }

func supportQuestion(r *rand.Rand, turn int) string {
	if turn > 0 {
		return pick(r,
			"That didn't work. I still see the same error after following your steps. What else can I try?",
			"Thanks. Can you also explain how this affects our bill this month?",
			"Our admin is out today. Is there a way to do this without owner permissions?",
			"Can you summarise the steps as a short checklist I can forward to my team?",
		) + fmt.Sprintf(" (ticket ref %d)", r.IntN(90000)+10000)
	}
	product := pick(r, "object storage", "managed Postgres", "Kubernetes service", "CDN", "serverless functions")
	issue := pick(r,
		"requests started failing with 503 errors after we rotated our access keys",
		"our invoice shows charges for resources we deleted two weeks ago",
		"latency doubled since yesterday in the eu-west region",
		"we cannot add a new team member; the invite email never arrives",
		"the dashboard says our quota is exceeded but usage looks low",
	)
	return fmt.Sprintf("Hi, we use your %s and %s. Account id ac-%06d, region %s. We have tried restarting and "+
		"re-deploying. Error excerpt: \"%s\". What should we do next?",
		product, issue, r.IntN(1000000), pick(r, "us-east-1", "eu-west-2", "ap-south-1"),
		pick(r, "upstream connect error or disconnect/reset before headers", "AccessDenied: signature mismatch",
			"quota_exceeded: project limit reached", "timeout after 30000ms"))
}

func analyticsQuestion(r *rand.Rand, turn int) string {
	if turn > 0 {
		return pick(r,
			"Now break that down by region and only include the last 90 days.",
			"Can you rewrite it to use a CTE and explain each step briefly?",
			"The query is slow on the warehouse. How would you index or restructure it?",
			"Also exclude internal test accounts (email ending in @globex.test).",
		)
	}
	metric := pick(r, "monthly recurring revenue", "churned customers", "average order value", "refund rate", "active seats")
	dim := pick(r, "plan tier", "sales region", "acquisition channel", "product category", "account manager")
	return fmt.Sprintf("Write a query that returns %s by %s for %s, sorted descending, top %d rows. "+
		"Treat cancelled orders as excluded and use UTC dates.", metric, dim,
		pick(r, "the last quarter", "fiscal year 2025", "each week of the last two months"), 5+r.IntN(20))
}

func docsQuestion(r *rand.Rand, turn int) string {
	if turn > 0 {
		return pick(r,
			"Can you show the same thing as a YAML workflow definition?",
			"What permissions does the service account need for that?",
			"How do I test this locally before deploying it?",
			"Is there a limit on how many of these I can run in parallel?",
		)
	}
	return pick(r,
		"How do I trigger a workflow when a file lands in an S3 bucket and retry failed steps with backoff?",
		"What is the difference between a step timeout and a workflow deadline, and which one should I set?",
		"How can I pass secrets to a step without them showing up in the run logs?",
		"Our scheduled workflow runs twice at the DST change. How do I configure the schedule correctly?",
		"How do I version a workflow so running instances keep the old definition while new runs use the new one?",
	) + fmt.Sprintf(" We're on Orbit %d.%d.", 3+r.IntN(2), r.IntN(10))
}

func legalQuestion(r *rand.Rand, turn int) string {
	if turn > 0 {
		return pick(r,
			"Propose redlines for the riskiest clause you identified, in contract language.",
			"How would this clause be interpreted if the counterparty is acquired?",
			"Summarise the position we should take in negotiation in three bullets.",
		)
	}
	clauses := []string{
		"Limitation of Liability. In no event shall either party be liable for any indirect, incidental, special, consequential or punitive damages, or any loss of profits or revenue, whether incurred directly or indirectly, or any loss of data, use, goodwill, or other intangible losses. Each party's aggregate liability arising out of or related to this Agreement shall not exceed the amounts paid by Customer in the twelve (12) months preceding the claim.",
		"Indemnification. Vendor shall defend Customer against any claim by a third party alleging that the Services infringe any intellectual property right, and shall indemnify Customer for damages finally awarded, provided Customer promptly notifies Vendor, gives Vendor sole control of the defence and settlement, and provides reasonable assistance at Vendor's expense.",
		"Term and Termination. This Agreement renews automatically for successive one-year terms unless either party gives notice of non-renewal at least ninety (90) days before the end of the then-current term. Either party may terminate for material breach not cured within thirty (30) days of written notice.",
		"Data Processing. Vendor may process Customer Data solely to provide the Services and may engage sub-processors upon fifteen (15) days' prior notice. Vendor shall notify Customer of a Personal Data Breach without undue delay and in any event within seventy-two (72) hours of becoming aware of it.",
	}
	return "Please review this clause from a vendor agreement and flag risks for us as the customer:\n\n" +
		clauses[r.IntN(len(clauses))]
}

func chatQuestion(r *rand.Rand, turn int) string {
	if turn > 0 {
		return pick(r, "Shorter please.", "Can you give an example?", "Why is that?", "And what are the downsides?")
	}
	return pick(r,
		"Draft a two-sentence status update about the search latency project being one week late.",
		"What is a good agenda for a 30 minute sprint retrospective?",
		"Explain the difference between p95 and average latency to a product manager.",
		"Suggest three names for an internal tool that summarises incident reports.",
		"How should I prepare for a design review of a new caching layer?",
	)
}

// analyticsSystem is a schema-plus-few-shot SQL prompt (about 4.7 KB).
func analyticsSystem() string {
	var b strings.Builder
	b.WriteString(`You are Globex's analytics assistant. You translate business questions into a single, correct PostgreSQL 15 query over the warehouse schema below.
Rules:
- Return only SQL in a fenced code block, followed by at most two sentences of explanation.
- Use explicit JOINs with ON clauses; never use NATURAL JOIN or SELECT *.
- All timestamps are stored in UTC. Use date_trunc for bucketing and half-open ranges (>= start AND < end).
- Monetary amounts are stored in cents as BIGINT; present them in dollars with two decimals.
- Exclude rows where is_test = true unless the user explicitly asks for test data.
- If the question is ambiguous, state the assumption you made in the explanation.

Schema:
`)
	tables := []struct {
		name string
		cols []string
	}{
		{"accounts", []string{"account_id BIGINT PRIMARY KEY", "name TEXT", "created_at TIMESTAMPTZ", "region TEXT", "segment TEXT", "account_manager_id BIGINT", "is_test BOOLEAN"}},
		{"users", []string{"user_id BIGINT PRIMARY KEY", "account_id BIGINT REFERENCES accounts", "email TEXT", "role TEXT", "created_at TIMESTAMPTZ", "last_seen_at TIMESTAMPTZ"}},
		{"plans", []string{"plan_id INT PRIMARY KEY", "name TEXT", "tier TEXT", "monthly_price_cents BIGINT", "seat_limit INT"}},
		{"subscriptions", []string{"subscription_id BIGINT PRIMARY KEY", "account_id BIGINT REFERENCES accounts", "plan_id INT REFERENCES plans", "status TEXT", "started_at TIMESTAMPTZ", "cancelled_at TIMESTAMPTZ", "seats INT"}},
		{"invoices", []string{"invoice_id BIGINT PRIMARY KEY", "account_id BIGINT REFERENCES accounts", "issued_at TIMESTAMPTZ", "due_at TIMESTAMPTZ", "amount_cents BIGINT", "status TEXT"}},
		{"payments", []string{"payment_id BIGINT PRIMARY KEY", "invoice_id BIGINT REFERENCES invoices", "paid_at TIMESTAMPTZ", "amount_cents BIGINT", "method TEXT"}},
		{"refunds", []string{"refund_id BIGINT PRIMARY KEY", "payment_id BIGINT REFERENCES payments", "refunded_at TIMESTAMPTZ", "amount_cents BIGINT", "reason TEXT"}},
		{"orders", []string{"order_id BIGINT PRIMARY KEY", "account_id BIGINT REFERENCES accounts", "placed_at TIMESTAMPTZ", "status TEXT", "channel TEXT", "total_cents BIGINT"}},
		{"order_items", []string{"order_item_id BIGINT PRIMARY KEY", "order_id BIGINT REFERENCES orders", "product_id BIGINT REFERENCES products", "quantity INT", "unit_price_cents BIGINT"}},
		{"products", []string{"product_id BIGINT PRIMARY KEY", "sku TEXT", "name TEXT", "category TEXT", "launched_at DATE"}},
		{"employees", []string{"employee_id BIGINT PRIMARY KEY", "name TEXT", "team TEXT", "region TEXT", "hired_at DATE"}},
		{"support_tickets", []string{"ticket_id BIGINT PRIMARY KEY", "account_id BIGINT REFERENCES accounts", "opened_at TIMESTAMPTZ", "closed_at TIMESTAMPTZ", "priority TEXT", "csat SMALLINT"}},
		{"usage_daily", []string{"account_id BIGINT REFERENCES accounts", "day DATE", "api_calls BIGINT", "storage_gb NUMERIC(12,2)", "active_users INT", "PRIMARY KEY (account_id, day)"}},
		{"marketing_touches", []string{"touch_id BIGINT PRIMARY KEY", "account_id BIGINT REFERENCES accounts", "touched_at TIMESTAMPTZ", "channel TEXT", "campaign TEXT"}},
	}
	for _, t := range tables {
		fmt.Fprintf(&b, "CREATE TABLE %s (\n  %s\n);\n", t.name, strings.Join(t.cols, ",\n  "))
	}
	b.WriteString(`
Examples:
Q: Monthly new accounts by region in 2025.
A: SELECT date_trunc('month', a.created_at) AS month, a.region, count(*) AS new_accounts FROM accounts a WHERE a.created_at >= '2025-01-01' AND a.created_at < '2026-01-01' AND NOT a.is_test GROUP BY 1, 2 ORDER BY 1, 2;
Q: Top 10 products by revenue last month.
A: SELECT p.name, sum(oi.quantity * oi.unit_price_cents) / 100.0 AS revenue FROM order_items oi JOIN orders o ON o.order_id = oi.order_id JOIN products p ON p.product_id = oi.product_id WHERE o.status <> 'cancelled' AND o.placed_at >= date_trunc('month', now()) - interval '1 month' AND o.placed_at < date_trunc('month', now()) GROUP BY p.name ORDER BY revenue DESC LIMIT 10;
Q: Share of invoices paid late, per quarter.
A: SELECT date_trunc('quarter', i.issued_at) AS quarter, avg((p.paid_at > i.due_at)::int) AS late_share FROM invoices i JOIN payments p ON p.invoice_id = i.invoice_id GROUP BY 1 ORDER BY 1;
Q: Accounts whose API usage dropped more than 50% week over week.
A: WITH w AS (SELECT account_id, date_trunc('week', day) AS week, sum(api_calls) AS calls FROM usage_daily GROUP BY 1, 2) SELECT cur.account_id, prev.calls, cur.calls FROM w cur JOIN w prev ON prev.account_id = cur.account_id AND prev.week = cur.week - interval '1 week' WHERE cur.week = date_trunc('week', now()) - interval '1 week' AND cur.calls < 0.5 * prev.calls;
Q: Median CSAT by ticket priority for enterprise accounts.
A: SELECT t.priority, percentile_cont(0.5) WITHIN GROUP (ORDER BY t.csat) AS median_csat FROM support_tickets t JOIN accounts a ON a.account_id = t.account_id WHERE a.segment = 'enterprise' AND NOT a.is_test AND t.csat IS NOT NULL GROUP BY t.priority;
`)
	return b.String()
}

// docsSystem is a product-documentation preamble (about 3.4 KB).
func docsSystem() string {
	sections := []struct{ title, body string }{
		{"Workflows", "A workflow is a directed acyclic graph of steps defined in YAML or with the Python SDK. Each workflow has a name, a version and an optional schedule. Publishing a new version never changes runs that already started: a run is pinned to the version it was created with. Use `orbit publish` to upload a definition and `orbit runs list` to inspect runs."},
		{"Triggers", "Runs start from a schedule (cron syntax, evaluated in the workflow's time zone), an HTTP webhook, a message on a queue, or an object-storage event. Object-storage triggers support prefix and suffix filters and deliver the bucket, key and etag as run inputs. Duplicate events within the deduplication window (default 5 minutes) start only one run."},
		{"Steps", "A step runs a container image or a built-in action. Steps declare inputs, outputs and resources. Outputs larger than 256 KiB must be written to the artifact store and passed by reference. Steps in the same stage run in parallel up to the workflow's concurrency limit."},
		{"Retries and timeouts", "Each step can set `retry: {max_attempts, backoff: exponential|fixed, initial_interval, max_interval}`. A step timeout bounds one attempt; the workflow deadline bounds the entire run including retries and waiting time. When the deadline passes the run is cancelled and compensation steps run if defined."},
		{"Secrets", "Secrets are stored in the Orbit vault and referenced as `${{ secrets.NAME }}`. Secret values are injected as environment variables or files and are masked in logs. Rotating a secret affects new attempts only. Access to secrets is granted per workflow through the `secrets.read` permission."},
		{"Permissions", "Orbit uses role-based access control. Built-in roles are viewer, operator, developer and admin. Service accounts can be granted roles on a folder or a single workflow. Triggering a run requires `runs.create`; reading logs requires `runs.read`; publishing requires `workflows.write`."},
		{"Scheduling and time zones", "Schedules are evaluated in the IANA time zone configured on the workflow (default UTC). During daylight-saving transitions a skipped local time does not fire and a repeated local time fires once, unless `dst_policy: fire_twice` is set. Use UTC schedules for jobs that must run exactly every 24 hours."},
		{"Local development", "`orbit dev` runs a workflow locally with containers on your machine, using a local artifact store and a fake vault populated from a .env file. Triggers can be simulated with `orbit dev trigger --event file.json`. Local runs are not visible in the web console."},
		{"Limits", "Default limits per project: 200 concurrent runs, 1,000 steps per run, 50 parallel steps per stage, 30 day log retention, and 10,000 webhook requests per minute. Limits can be raised through a support request for enterprise plans."},
		{"Observability", "Each run emits OpenTelemetry traces with one span per step attempt. Metrics are exported in Prometheus format and include run duration, step failures by error class and queue wait time. Logs are searchable for 30 days and can be streamed to an external sink."},
	}
	var b strings.Builder
	b.WriteString("You are the documentation assistant for Initech Orbit, a workflow automation platform. Answer using only the reference material below. " +
		"Quote configuration keys exactly, include a short example when it helps, and link to the relevant section title in brackets. " +
		"If the material does not cover the question, say so and suggest contacting support.\n\nReference material:\n")
	for _, s := range sections {
		fmt.Fprintf(&b, "\n## %s\n%s\n", s.title, s.body)
	}
	return b.String()
}
