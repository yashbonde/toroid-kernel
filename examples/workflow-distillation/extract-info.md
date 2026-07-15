---
name: Extract Key Information
description: Parse documents and extract structured facts with high accuracy on small models
---

## Objective

Extract key information from a document (log file, report, email, etc.) and return it as clean, structured JSON. This skill is optimized for 4B models; be very explicit about the format.

## Process

1. **Read the document** using the `read` tool
2. **Identify the document type** (log file, report, email, code)
3. **Extract facts** according to the type (see decision rules below)
4. **Return JSON** in the exact format specified

## Decision Rules

### If document is a log file
Extract:
- `errors`: list of unique ERROR lines (deduplicated by prefix)
- `warnings`: list of unique WARNING lines
- `info_count`: total INFO lines
- `errors_count`: total ERROR count
- `time_range`: earliest and latest timestamps

Format:
```json
{
  "type": "log",
  "errors": ["Error: connection timeout", "Error: out of memory"],
  "warnings": ["Warning: deprecated API"],
  "info_count": 150,
  "error_count": 5,
  "time_range": {"earliest": "2026-01-01T10:00:00Z", "latest": "2026-01-01T11:30:00Z"}
}
```

### If document is a report
Extract:
- `title`: report title
- `sections`: list of section headings
- `key_findings`: top 3 findings as strings
- `recommendations`: top 3 recommendations
- `metrics`: any numbers (counts, percentages, sums)

Format:
```json
{
  "type": "report",
  "title": "Q1 Performance Review",
  "sections": ["Executive Summary", "Detailed Analysis", "Recommendations"],
  "key_findings": ["Revenue up 20%", "Customer churn decreased", "Team expanded"],
  "recommendations": ["Invest in automation", "Hire 5 engineers"],
  "metrics": {"revenue": 1000000, "churn_pct": 5.2}
}
```

### If document is code
Extract:
- `language`: programming language
- `functions`: list of function/method names (top 10)
- `imports`: list of imported modules (top 10)
- `comments`: any interesting comments (top 5)
- `lines_of_code`: approximate count

Format:
```json
{
  "type": "code",
  "language": "python",
  "functions": ["process_data", "validate_input", "save_result"],
  "imports": ["json", "os", "re"],
  "comments": ["TODO: optimize this loop"],
  "lines_of_code": 250
}
```

## Rules

1. **Be concise.** No prose, only structured data.
2. **Deduplicate.** Merge similar strings (e.g., "timeout" and "connection timeout" → one entry).
3. **Limit lists to 10 items.** If there are more, take the top 10 by frequency.
4. **Return valid JSON only.** No markdown, no explanation.
5. **If unsure of a field, omit it.** Don't guess or fabricate data.
6. **Case-insensitive matching.** Treat "ERROR" and "Error" the same.

## Example

Input: `/var/log/app.log` containing:
```
[2026-01-01 10:00:00] INFO: Started server
[2026-01-01 10:01:00] ERROR: Connection timeout
[2026-01-01 10:02:00] WARNING: Deprecated API used
[2026-01-01 10:03:00] INFO: Request processed
[2026-01-01 10:04:00] ERROR: Connection timeout
```

Output:
```json
{
  "type": "log",
  "errors": ["ERROR: Connection timeout"],
  "warnings": ["WARNING: Deprecated API used"],
  "info_count": 2,
  "error_count": 2,
  "time_range": {
    "earliest": "2026-01-01T10:00:00Z",
    "latest": "2026-01-01T10:04:00Z"
  }
}
```
