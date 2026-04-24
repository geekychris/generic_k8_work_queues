package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// --- Request / Response types ---

type ProcessRequest struct {
	JobID   string          `json:"job_id"`
	Payload json.RawMessage `json:"payload"`
}

type ProcessResponse struct {
	Success bool        `json:"success"`
	Result  interface{} `json:"result,omitempty"`
	Error   string      `json:"error,omitempty"`
	Logs    []LogEntry  `json:"logs,omitempty"`
}

type LogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// ReviewPayload is what the webhook service sends.
type ReviewPayload struct {
	Action       string       `json:"action"`
	RepoOwner    string       `json:"repo_owner"`
	RepoName     string       `json:"repo_name"`
	CloneURL     string       `json:"clone_url"`
	Ref          string       `json:"ref"`
	PRNumber     int          `json:"pr_number,omitempty"`
	PRTitle      string       `json:"pr_title,omitempty"`
	PRBody       string       `json:"pr_body,omitempty"`
	Sender       string       `json:"sender"`
	CommitSHA    string       `json:"commit_sha,omitempty"`
	FilesChanged []FileChange `json:"files_changed,omitempty"`
}

type FileChange struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Patch    string `json:"patch,omitempty"`
}

// --- GitHub API types ---

type GHPullRequest struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type GHPRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Patch     string `json:"patch"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type GHReviewRequest struct {
	CommitID string            `json:"commit_id,omitempty"`
	Event    string            `json:"event"`
	Body     string            `json:"body"`
	Comments []GHReviewComment `json:"comments,omitempty"`
}

type GHReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Side string `json:"side,omitempty"`
	Body string `json:"body"`
}

type GHReviewResponse struct {
	ID int `json:"id"`
}

type GHReviewCommentResponse struct {
	ID int `json:"id"`
}

// --- Structured review from LLM ---

type StructuredReview struct {
	Summary  string          `json:"summary"`
	Verdict  string          `json:"verdict"`
	Comments []ReviewComment `json:"comments"`
}

type ReviewComment struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Severity   string `json:"severity"` // critical, warning, suggestion, praise
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"` // replacement code for the line(s)
}

// --- OpenClaude types ---

type OpenClaudeResult struct {
	Type       string  `json:"type"`
	Result     string  `json:"result"`
	IsError    bool    `json:"is_error"`
	DurationMs int     `json:"duration_ms"`
	StopReason string  `json:"stop_reason"`
	TotalCost  float64 `json:"total_cost_usd"`
	ModelUsage map[string]struct {
		InputTokens  int     `json:"inputTokens"`
		OutputTokens int     `json:"outputTokens"`
		CostUSD      float64 `json:"costUSD"`
	} `json:"modelUsage"`
}

// --- Globals ---

var (
	openclaudeBin   string
	openclaudeModel string
	provider        string
	ollamaURL       string
	githubToken     string
	postReviews     bool
)

func main() {
	openclaudeBin = envOr("OPENCLAUDE_BIN", "openclaude")
	openclaudeModel = envOr("OPENCLAUDE_MODEL", "llama3.2")
	provider = envOr("OPENCLAUDE_PROVIDER", "ollama")
	ollamaURL = envOr("OLLAMA_URL", "http://host.docker.internal:11434")
	githubToken = os.Getenv("GITHUB_TOKEN")
	postReviews = envOr("POST_REVIEWS", "true") == "true"
	listenPort := envOr("PORT", "8080")

	log.Printf("codereview-worker starting provider=%s model=%s github_token=%v post_reviews=%v port=%s",
		provider, openclaudeModel, githubToken != "", postReviews, listenPort)

	http.HandleFunc("/process", handleProcess)
	http.HandleFunc("/health", handleHealth)

	log.Printf("listening on :%s", listenPort)
	if err := http.ListenAndServe(":"+listenPort, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func handleProcess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, ProcessResponse{Error: fmt.Sprintf("invalid request: %v", err)})
		return
	}

	var payload ReviewPayload
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		respond(w, ProcessResponse{Error: fmt.Sprintf("invalid payload: %v", err)})
		return
	}

	var logs []LogEntry
	logf := func(level, format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		log.Printf("job_id=%s [%s] %s", req.JobID, level, msg)
		logs = append(logs, LogEntry{Level: level, Message: msg})
	}

	logf("info", "=== Code Review Starting ===")
	logf("info", "Action: %s | Repo: %s/%s | PR: #%d | Author: %s",
		payload.Action, payload.RepoOwner, payload.RepoName, payload.PRNumber, payload.Sender)

	// --- Step 1: Fetch PR details from GitHub API ---
	var prFiles []GHPRFile
	var prInfo *GHPullRequest

	if githubToken != "" && payload.PRNumber > 0 {
		logf("info", "Fetching PR #%d from GitHub API...", payload.PRNumber)

		var err error
		prInfo, err = fetchPRInfo(payload.RepoOwner, payload.RepoName, payload.PRNumber)
		if err != nil {
			logf("warn", "Failed to fetch PR info: %v", err)
		} else {
			logf("info", "PR: %s (base: %s <- head: %s @ %s)",
				prInfo.Title, prInfo.Base.Ref, prInfo.Head.Ref, prInfo.Head.SHA[:8])
			if payload.PRTitle == "" {
				payload.PRTitle = prInfo.Title
			}
			if payload.PRBody == "" {
				payload.PRBody = prInfo.Body
			}
			if payload.CommitSHA == "" {
				payload.CommitSHA = prInfo.Head.SHA
			}
		}

		prFiles, err = fetchPRFiles(payload.RepoOwner, payload.RepoName, payload.PRNumber)
		if err != nil {
			logf("warn", "Failed to fetch PR files: %v", err)
		} else {
			logf("info", "Fetched %d changed files", len(prFiles))
			payload.FilesChanged = make([]FileChange, len(prFiles))
			for i, f := range prFiles {
				payload.FilesChanged[i] = FileChange{
					Filename: f.Filename,
					Status:   f.Status,
					Patch:    f.Patch,
				}
				logf("info", "  %s [%s] +%d -%d", f.Filename, f.Status, f.Additions, f.Deletions)
			}
		}
	} else if githubToken == "" {
		logf("warn", "GITHUB_TOKEN not set — using webhook data only")
	}

	// --- Step 2: Clone the repo ---
	var cloneDir string
	if githubToken != "" && payload.CloneURL != "" {
		logf("info", "Cloning %s/%s ...", payload.RepoOwner, payload.RepoName)
		var err error
		cloneDir, err = cloneRepo(payload.CloneURL, payload.Ref, payload.CommitSHA)
		if err != nil {
			logf("warn", "Clone failed: %v", err)
		} else {
			defer os.RemoveAll(cloneDir)
			langs := detectLanguages(payload.FilesChanged)
			logf("info", "Cloned OK. Languages: %s", strings.Join(langs, ", "))
		}
	}

	// --- Step 3: Build prompt and call LLM for structured review ---
	prompt := buildStructuredReviewPrompt(payload, cloneDir)
	logf("info", "Built review prompt (%d chars)", len(prompt))
	logf("info", "Calling openclaude (provider=%s, model=%s)...", provider, openclaudeModel)

	start := time.Now()
	rawReview, ocResult, err := callOpenClaude(prompt, cloneDir)
	duration := time.Since(start)

	if err != nil {
		logf("error", "openclaude failed after %v: %v", duration, err)
		respond(w, ProcessResponse{Error: fmt.Sprintf("openclaude failed: %v", err), Logs: logs})
		return
	}

	logf("info", "openclaude responded in %v (%d chars)", duration, len(rawReview))
	if ocResult != nil {
		logf("info", "Stats: duration=%dms cost=$%.6f", ocResult.DurationMs, ocResult.TotalCost)
		for model, usage := range ocResult.ModelUsage {
			logf("info", "  model=%s input=%d output=%d tokens", model, usage.InputTokens, usage.OutputTokens)
		}
	}

	// --- Step 4: Parse structured review ---
	logf("info", "--- Raw LLM Output ---")
	for _, line := range strings.Split(rawReview, "\n") {
		if strings.TrimSpace(line) != "" {
			logf("info", "  %s", line)
		}
	}
	logf("info", "--- End Raw Output ---")

	review := parseStructuredReview(rawReview)
	logf("info", "Parsed review: verdict=%s, %d inline comments", review.Verdict, len(review.Comments))
	logf("info", "Summary: %s", review.Summary)

	for _, c := range review.Comments {
		icon := severityIcon(c.Severity)
		logf("info", "  %s %s:%d [%s] %s", icon, c.File, c.Line, c.Severity, c.Message)
		if c.Suggestion != "" {
			logf("info", "    suggestion: %s", truncate(c.Suggestion, 100))
		}
	}

	// --- Step 5: Post to GitHub ---
	var reviewPosted bool
	var commentsPosted int
	if postReviews && githubToken != "" && payload.PRNumber > 0 {
		logf("info", "Posting review to GitHub PR #%d...", payload.PRNumber)

		posted, nComments, err := postStructuredReview(
			payload.RepoOwner, payload.RepoName, payload.PRNumber,
			payload.CommitSHA, review, payload.FilesChanged,
		)
		if err != nil {
			logf("error", "Failed to post review: %v", err)
		} else {
			logf("info", "Posted review (verdict=%s) with %d inline comments", review.Verdict, nComments)
			reviewPosted = posted
			commentsPosted = nComments
		}
	}

	logf("info", "=== Code Review Complete ===")

	result := map[string]interface{}{
		"action":          payload.Action,
		"repo":            payload.RepoOwner + "/" + payload.RepoName,
		"pr_number":       payload.PRNumber,
		"pr_title":        payload.PRTitle,
		"summary":         review.Summary,
		"verdict":         review.Verdict,
		"comments":        review.Comments,
		"review_posted":   reviewPosted,
		"comments_posted": commentsPosted,
		"provider":        provider,
		"model":           openclaudeModel,
		"duration_ms":     duration.Milliseconds(),
		"files_reviewed":  len(payload.FilesChanged),
		"worker":          hostname(),
	}
	if ocResult != nil {
		result["openclaude_cost_usd"] = ocResult.TotalCost
	}

	respond(w, ProcessResponse{Success: true, Result: result, Logs: logs})
}

// --- Structured review prompt ---

func buildStructuredReviewPrompt(p ReviewPayload, cloneDir string) string {
	var sb strings.Builder
	langs := detectLanguages(p.FilesChanged)

	sb.WriteString("You are an expert code reviewer specializing in ")
	sb.WriteString(strings.Join(langs, ", "))
	sb.WriteString(".\n\n")

	sb.WriteString(`Review the code changes below.

IMPORTANT RULES:
1. Respond with ONLY valid JSON. No text before or after.
2. The "suggestion" field must contain the EXACT replacement code for the line. NOT a comment explaining what to do. It must be code that directly replaces the problematic line.
3. The "line" must be the line number shown in the numbered source listing below.

Example of a GOOD suggestion for a SQL injection on line 29:
{
  "file": "Example.java",
  "line": 29,
  "severity": "critical",
  "message": "SQL injection: user input concatenated into query string",
  "suggestion": "String sql = \"SELECT * FROM users WHERE \" + field + \" = ?\";\nreturn entityManager.createNativeQuery(sql, User.class).setParameter(1, value).getResultList();"
}

Example of a BAD suggestion (do NOT do this):
{
  "suggestion": "// Use parameterized queries to prevent SQL injection"
}

The suggestion MUST be actual runnable code, not a comment.

JSON format:
{"summary":"one sentence","verdict":"approve or request_changes or comment","comments":[{"file":"path","line":123,"severity":"critical or warning or suggestion or praise","message":"what is wrong","suggestion":"replacement code"}]}

Severities: critical = security/crash/data-loss, warning = bugs/leaks, suggestion = style/perf, praise = good code.

`)

	// Language-specific guidance
	for _, lang := range langs {
		switch lang {
		case "Go":
			sb.WriteString("Go checks: unchecked errors, goroutine leaks, defer in loops, context propagation, race conditions.\n")
		case "Java":
			sb.WriteString("Java checks: SQL injection, unclosed resources (try-with-resources), null safety, thread safety, hardcoded secrets, command injection.\n")
		case "Python":
			sb.WriteString("Python checks: bare except, mutable defaults, SQL injection via string format, eval/pickle, missing type hints.\n")
		}
	}

	sb.WriteString("\n")

	if p.PRTitle != "" {
		sb.WriteString(fmt.Sprintf("PR #%d: %s\n", p.PRNumber, p.PRTitle))
	}
	if p.PRBody != "" {
		sb.WriteString(fmt.Sprintf("Description: %s\n", p.PRBody))
	}
	sb.WriteString(fmt.Sprintf("Repo: %s/%s | Author: %s | Branch: %s\n\n", p.RepoOwner, p.RepoName, p.Sender, p.Ref))

	for _, f := range p.FilesChanged {
		sb.WriteString(fmt.Sprintf("=== FILE: %s (%s) ===\n", f.Filename, f.Status))

		// Full file from clone for context
		if cloneDir != "" && f.Status != "removed" {
			content := readFileFromClone(cloneDir, f.Filename)
			if content != "" {
				sb.WriteString("--- Full file (with line numbers) ---\n")
				for i, line := range strings.Split(content, "\n") {
					sb.WriteString(fmt.Sprintf("%d: %s\n", i+1, line))
				}
				sb.WriteString("--- End file ---\n")
			}
		}

		if f.Patch != "" {
			patch := f.Patch
			if len(patch) > 5000 {
				patch = patch[:5000] + "\n... (truncated)"
			}
			sb.WriteString("--- Diff ---\n")
			sb.WriteString(patch)
			sb.WriteString("\n--- End diff ---\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\nRespond with JSON only. Include at least a summary and verdict. Include praise comments for good patterns too.\n")
	return sb.String()
}

// --- Parse structured review from LLM output ---

func parseStructuredReview(raw string) StructuredReview {
	jsonStr := extractJSON(raw)

	var review StructuredReview

	// First attempt: strict parse
	if err := json.Unmarshal([]byte(jsonStr), &review); err != nil {
		// LLMs often produce JSON with unescaped control chars in string values.
		// Use a decoder that tolerates this somewhat, and also try stripping
		// the suggestion fields (which are most likely to be malformed).
		log.Printf("Strict JSON parse failed: %v — trying lenient parse", err)

		// Strategy: extract fields with regex since the structure is known
		review = regexParseReview(jsonStr)
	}

	if review.Summary == "" && review.Verdict == "" && len(review.Comments) == 0 {
		log.Printf("All parsing failed, falling back to plain text")
		// Never use raw JSON as the summary — strip it to plain text
		summary := raw
		if strings.HasPrefix(strings.TrimSpace(summary), "{") {
			summary = "Code review completed (structured output could not be parsed)"
		}
		return StructuredReview{
			Summary: summary,
			Verdict: extractVerdict(raw),
		}
	}

	// Normalize comment severities (LLMs sometimes use different words)
	for i := range review.Comments {
		review.Comments[i].Severity = normalizeSeverity(review.Comments[i].Severity)
	}

	// Normalize verdict
	review.Verdict = strings.ToLower(review.Verdict)
	if review.Verdict != "approve" && review.Verdict != "request_changes" && review.Verdict != "comment" {
		review.Verdict = extractVerdict(review.Summary + " " + review.Verdict)
	}

	return review
}

// regexParseReview extracts review data using regex when JSON parsing fails.
func regexParseReview(raw string) StructuredReview {
	review := StructuredReview{}

	// Extract summary
	if m := regexp.MustCompile(`"summary"\s*:\s*"([^"]*?)"`).FindStringSubmatch(raw); len(m) > 1 {
		review.Summary = m[1]
	}

	// Extract verdict
	if m := regexp.MustCompile(`"verdict"\s*:\s*"([^"]*?)"`).FindStringSubmatch(raw); len(m) > 1 {
		review.Verdict = m[1]
	}

	// Extract individual comment blocks by finding each { "file": pattern
	commentRe := regexp.MustCompile(`\{\s*"file"\s*:\s*"([^"]+)"\s*,\s*"line"\s*:\s*(\d+)\s*,\s*"severity"\s*:\s*"([^"]+)"\s*,\s*"message"\s*:\s*"([^"]*(?:\\.[^"]*)*)"`)
	matches := commentRe.FindAllStringSubmatch(raw, -1)

	for _, m := range matches {
		if len(m) >= 5 {
			line, _ := strconv.Atoi(m[2])
			comment := ReviewComment{
				File:     m[1],
				Line:     line,
				Severity: m[3],
				Message:  strings.ReplaceAll(m[4], `\"`, `"`),
			}

			// Try to extract suggestion for this comment
			// Look for "suggestion": "..." after this match position
			idx := strings.Index(raw, m[0])
			if idx >= 0 {
				after := raw[idx+len(m[0]):]
				if sugM := regexp.MustCompile(`"suggestion"\s*:\s*"((?:[^"\\]|\\.)*)"`).FindStringSubmatch(after); len(sugM) > 1 {
					suggestion := sugM[1]
					suggestion = strings.ReplaceAll(suggestion, `\n`, "\n")
					suggestion = strings.ReplaceAll(suggestion, `\"`, `"`)
					suggestion = strings.ReplaceAll(suggestion, `\\`, `\`)
					comment.Suggestion = suggestion
				}
			}

			review.Comments = append(review.Comments, comment)
		}
	}

	log.Printf("Regex parse: summary=%q verdict=%q comments=%d", truncate(review.Summary, 50), review.Verdict, len(review.Comments))
	return review
}

// extractJSON finds the first complete JSON object in the text.
func extractJSON(text string) string {
	// Try the whole thing first
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "{") {
		return text
	}

	// Look for JSON block in markdown
	re := regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(\\{.*?\\})\\s*```")
	if m := re.FindStringSubmatch(text); len(m) > 1 {
		return m[1]
	}

	// Find first { and matching }
	start := strings.Index(text, "{")
	if start == -1 {
		return text
	}

	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}

	return text[start:]
}

// --- Post structured review to GitHub ---

func postStructuredReview(owner, repo string, prNumber int, commitSHA string,
	review StructuredReview, files []FileChange) (bool, int, error) {

	// Build inline comments
	var ghComments []GHReviewComment
	for _, c := range review.Comments {
		if c.File == "" || c.Line <= 0 {
			continue
		}

		// Verify the line is in the diff (GitHub rejects comments on unchanged lines)
		if !isLineInDiff(c.File, c.Line, files) {
			log.Printf("Skipping comment on %s:%d — not in diff", c.File, c.Line)
			continue
		}

		body := formatCommentBody(c)
		ghComments = append(ghComments, GHReviewComment{
			Path: c.File,
			Line: c.Line,
			Side: "RIGHT", // comment on the new version of the file
			Body: body,
		})
	}

	// Build review body with summary
	var bodyParts []string
	bodyParts = append(bodyParts, fmt.Sprintf("## KQueue Code Review\n\n%s", review.Summary))

	// Count severities
	counts := map[string]int{}
	for _, c := range review.Comments {
		counts[c.Severity]++
	}
	if len(counts) > 0 {
		bodyParts = append(bodyParts, fmt.Sprintf("\n\n### Review Summary\n| Severity | Count |\n|----------|-------|\n| %s Critical | %d |\n| %s Warning | %d |\n| %s Suggestion | %d |\n| %s Praise | %d |",
			severityIcon("critical"), counts["critical"],
			severityIcon("warning"), counts["warning"],
			severityIcon("suggestion"), counts["suggestion"],
			severityIcon("praise"), counts["praise"],
		))
	}

	// Add skipped comments (not in diff) as a list in the body
	var skippedComments []ReviewComment
	for _, c := range review.Comments {
		if c.File == "" || c.Line <= 0 || !isLineInDiff(c.File, c.Line, files) {
			skippedComments = append(skippedComments, c)
		}
	}
	if len(skippedComments) > 0 {
		bodyParts = append(bodyParts, fmt.Sprintf("\n\n### Additional Comments (%d not in diff)\n", len(skippedComments)))
		for _, c := range skippedComments {
			icon := severityIcon(c.Severity)
			bodyParts = append(bodyParts, fmt.Sprintf("- %s **%s** `%s:%d` — %s", icon, strings.ToUpper(c.Severity), c.File, c.Line, c.Message))
		}
	}

	bodyParts = append(bodyParts, fmt.Sprintf("\n\n---\n*Reviewed by KQueue codereview worker (%s/%s)*", provider, openclaudeModel))

	event := "COMMENT"
	switch review.Verdict {
	case "approve":
		event = "APPROVE"
	case "request_changes":
		event = "REQUEST_CHANGES"
	}

	ghReview := GHReviewRequest{
		CommitID: commitSHA,
		Event:    event,
		Body:     strings.Join(bodyParts, ""),
		Comments: ghComments,
	}

	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, prNumber)
	respBody, status, err := githubAPI("POST", path, ghReview)
	if err != nil {
		return false, 0, err
	}

	// Fallback: can't REQUEST_CHANGES on own PR
	if status == 422 && event != "COMMENT" {
		log.Printf("Falling back to COMMENT (cannot %s on own PR)", event)
		ghReview.Event = "COMMENT"
		respBody, status, err = githubAPI("POST", path, ghReview)
		if err != nil {
			return false, 0, err
		}
	}

	if status != 200 {
		return false, 0, fmt.Errorf("GitHub API returned %d: %s", status, truncate(string(respBody), 300))
	}

	// Parse response to get review ID for reactions
	var reviewResp GHReviewResponse
	json.Unmarshal(respBody, &reviewResp)

	// Add reactions to the review comments
	if reviewResp.ID > 0 {
		go addReactionsToReviewComments(owner, repo, prNumber, reviewResp.ID, review.Comments, ghComments)
	}

	return true, len(ghComments), nil
}

// addReactionsToReviewComments fetches the posted comments and adds reactions based on severity.
func addReactionsToReviewComments(owner, repo string, prNumber, reviewID int,
	comments []ReviewComment, ghComments []GHReviewComment) {

	// Fetch the review comments to get their IDs
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d/comments", owner, repo, prNumber, reviewID)
	body, status, err := githubAPI("GET", path, nil)
	if err != nil || status != 200 {
		log.Printf("Failed to fetch review comments for reactions: %v (status=%d)", err, status)
		return
	}

	var postedComments []struct {
		ID   int    `json:"id"`
		Path string `json:"path"`
		Line int    `json:"line"`
	}
	if err := json.Unmarshal(body, &postedComments); err != nil {
		return
	}

	// Match posted comments to our review comments by file+line
	severityMap := map[string]string{}
	for _, c := range comments {
		key := fmt.Sprintf("%s:%d", c.File, c.Line)
		severityMap[key] = c.Severity
	}

	for _, pc := range postedComments {
		key := fmt.Sprintf("%s:%d", pc.Path, pc.Line)
		severity := severityMap[key]

		var reaction string
		switch severity {
		case "critical":
			reaction = "-1" // thumbs down
		case "warning":
			reaction = "confused"
		case "suggestion":
			reaction = "eyes"
		case "praise":
			reaction = "+1" // thumbs up
		default:
			continue
		}

		reactPath := fmt.Sprintf("/repos/%s/%s/pulls/comments/%d/reactions", owner, repo, pc.ID)
		githubAPI("POST", reactPath, map[string]string{"content": reaction})
	}
}

// formatCommentBody formats a review comment with severity badge and optional suggestion.
func formatCommentBody(c ReviewComment) string {
	var parts []string

	// Severity badge
	icon := severityIcon(c.Severity)
	badge := fmt.Sprintf("**%s %s**", icon, strings.ToUpper(c.Severity))
	parts = append(parts, badge)
	parts = append(parts, "")
	parts = append(parts, c.Message)

	// Add suggestion block if available
	if c.Suggestion != "" {
		parts = append(parts, "")
		parts = append(parts, "```suggestion")
		parts = append(parts, c.Suggestion)
		parts = append(parts, "```")
	}

	return strings.Join(parts, "\n")
}

// isLineInDiff checks if a line number appears in the diff for a file.
func isLineInDiff(filename string, line int, files []FileChange) bool {
	for _, f := range files {
		if f.Filename != filename || f.Patch == "" {
			continue
		}
		// Parse diff hunks to find valid line ranges
		lines := parseDiffLines(f.Patch)
		for _, l := range lines {
			if l == line {
				return true
			}
		}
	}
	return false
}

// parseDiffLines extracts the new-side line numbers from a unified diff patch.
func parseDiffLines(patch string) []int {
	var lines []int
	hunkRe := regexp.MustCompile(`@@\s+-\d+(?:,\d+)?\s+\+(\d+)(?:,(\d+))?\s+@@`)

	currentLine := 0
	for _, rawLine := range strings.Split(patch, "\n") {
		if m := hunkRe.FindStringSubmatch(rawLine); len(m) > 1 {
			currentLine, _ = strconv.Atoi(m[1])
			continue
		}
		if currentLine == 0 {
			continue
		}
		if strings.HasPrefix(rawLine, "-") {
			// Removed line — doesn't count in new file
			continue
		}
		if strings.HasPrefix(rawLine, "+") || !strings.HasPrefix(rawLine, "\\") {
			lines = append(lines, currentLine)
			currentLine++
		}
	}
	return lines
}

func normalizeSeverity(s string) string {
	switch strings.ToLower(s) {
	case "critical", "high", "error", "blocker":
		return "critical"
	case "warning", "medium", "warn", "major":
		return "warning"
	case "suggestion", "low", "minor", "info", "improvement", "nitpick", "nit":
		return "suggestion"
	case "praise", "positive", "good", "nice":
		return "praise"
	default:
		return "suggestion"
	}
}

func severityIcon(severity string) string {
	switch severity {
	case "critical":
		return "\U0001F6A8" // rotating light
	case "warning":
		return "\u26A0\uFE0F" // warning sign
	case "suggestion":
		return "\U0001F4A1" // light bulb
	case "praise":
		return "\u2B50" // star
	default:
		return "\U0001F4AC" // speech bubble
	}
}

// --- GitHub API ---

func githubAPI(method, path string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reqBody = bytes.NewReader(data)
	}

	url := "https://api.github.com" + path
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

func fetchPRInfo(owner, repo string, prNumber int) (*GHPullRequest, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	body, status, err := githubAPI("GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("GitHub API %d: %s", status, truncate(string(body), 200))
	}
	var pr GHPullRequest
	return &pr, json.Unmarshal(body, &pr)
}

func fetchPRFiles(owner, repo string, prNumber int) ([]GHPRFile, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", owner, repo, prNumber)
	body, status, err := githubAPI("GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("GitHub API %d: %s", status, truncate(string(body), 200))
	}
	var files []GHPRFile
	return files, json.Unmarshal(body, &files)
}

// --- Git clone ---

func cloneRepo(cloneURL, ref, sha string) (string, error) {
	workDir, err := os.MkdirTemp("", "codereview-*")
	if err != nil {
		return "", err
	}

	if githubToken != "" && strings.Contains(cloneURL, "github.com") {
		cloneURL = strings.Replace(cloneURL, "https://", fmt.Sprintf("https://x-access-token:%s@", githubToken), 1)
	}

	args := []string{"clone", "--depth=50"}
	if ref != "" && !strings.Contains(ref, "/") {
		args = append(args, "--branch", ref)
	}
	args = append(args, cloneURL, workDir)

	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(workDir)
		return "", fmt.Errorf("git clone: %v\n%s", err, truncate(string(output), 500))
	}

	if sha != "" {
		cmd = exec.Command("git", "-C", workDir, "checkout", sha)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("checkout sha failed: %s", truncate(string(out), 200))
		}
	}

	return workDir, nil
}

// --- Language detection ---

func detectLanguages(files []FileChange) []string {
	langSet := make(map[string]bool)
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.Filename))
		switch ext {
		case ".go":
			langSet["Go"] = true
		case ".java":
			langSet["Java"] = true
		case ".py":
			langSet["Python"] = true
		case ".js", ".jsx", ".ts", ".tsx":
			langSet["JavaScript/TypeScript"] = true
		case ".rb":
			langSet["Ruby"] = true
		case ".rs":
			langSet["Rust"] = true
		case ".c", ".h", ".cpp", ".hpp":
			langSet["C/C++"] = true
		case ".kt", ".kts":
			langSet["Kotlin"] = true
		case ".yaml", ".yml":
			langSet["YAML"] = true
		case ".sql":
			langSet["SQL"] = true
		case ".sh", ".bash":
			langSet["Shell"] = true
		case ".properties":
			langSet["Properties/Config"] = true
		}
	}
	var langs []string
	for l := range langSet {
		langs = append(langs, l)
	}
	if len(langs) == 0 {
		langs = append(langs, "unknown")
	}
	return langs
}

func readFileFromClone(cloneDir, filename string) string {
	path := filepath.Join(cloneDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := string(data)
	if len(content) > 8000 {
		content = content[:8000] + "\n... (truncated)"
	}
	return content
}

// --- OpenClaude ---

func callOpenClaude(prompt string, workDir string) (string, *OpenClaudeResult, error) {
	args := []string{
		"-p",
		"--bare",
		"--provider", provider,
		"--model", openclaudeModel,
		"--output-format", "json",
		"--no-session-persistence",
		"--disallowedTools", "Bash", "Edit", "Read", "Write", "Glob", "Grep",
		"Agent", "NotebookEdit", "WebFetch", "WebSearch",
	}

	if workDir != "" {
		args = append(args, "--add-dir", workDir)
	}

	cmd := exec.Command(openclaudeBin, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(os.Environ(),
		"OLLAMA_BASE_URL="+ollamaURL,
		"OPENAI_BASE_URL="+ollamaURL+"/v1",
		"OPENAI_MODEL="+openclaudeModel,
	)
	if workDir != "" {
		cmd.Dir = workDir
	}

	log.Printf("running: %s %s", openclaudeBin, strings.Join(args, " "))

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", nil, fmt.Errorf("openclaude exited %d: %s", exitErr.ExitCode(), truncate(string(exitErr.Stderr), 500))
		}
		return "", nil, fmt.Errorf("openclaude exec: %w", err)
	}

	var jsonLine string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[context]") {
			continue
		}
		if strings.HasPrefix(line, "{") {
			jsonLine = line
		}
	}

	if jsonLine == "" {
		return strings.TrimSpace(string(output)), nil, nil
	}

	var result OpenClaudeResult
	if err := json.Unmarshal([]byte(jsonLine), &result); err != nil {
		return strings.TrimSpace(string(output)), nil, nil
	}

	if result.IsError {
		return "", &result, fmt.Errorf("openclaude error: %s", result.Result)
	}

	return result.Result, &result, nil
}

// --- Verdict extraction (fallback for unstructured responses) ---

func extractVerdict(text string) string {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "request_changes") || strings.Contains(lower, "request changes") ||
		strings.Contains(lower, "critical") || strings.Contains(lower, "must be fixed") {
		return "request_changes"
	}
	if strings.Contains(lower, "approve") || strings.Contains(lower, "lgtm") || strings.Contains(lower, "looks good") {
		return "approve"
	}
	return "comment"
}

// --- Helpers ---

func respond(w http.ResponseWriter, resp ProcessResponse) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
