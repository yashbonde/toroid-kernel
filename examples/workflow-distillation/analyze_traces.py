#!/usr/bin/env python3
"""
Trace Analysis: Extract patterns from toroid NDJSON traces.

This script reads trace files and identifies:
1. Common tool call sequences
2. Decision trees (if result X, then call Y)
3. Tool argument patterns
4. Error rates and failure modes

Usage:
    python3 analyze_traces.py traces.jsonl > report.md
    python3 analyze_traces.py traces_*.jsonl --output report.json
"""

import json
import sys
import argparse
from collections import defaultdict, Counter
from typing import Dict, List, Any


def load_traces(file):
    """Load NDJSON trace file."""
    traces = []
    with open(file) as f:
        for line in f:
            if line.strip():
                traces.append(json.loads(line))
    return traces


def extract_sessions(traces: List[Dict]) -> Dict[str, List[Dict]]:
    """Group traces by session ID."""
    sessions = defaultdict(list)
    for trace in traces:
        sessions[trace["session_id"]].append(trace)
    return sessions


def extract_tool_sequences(sessions: Dict) -> List[List[str]]:
    """Extract ordered tool call sequences per session."""
    sequences = []
    for session_id, traces in sessions.items():
        tools = []
        for trace in traces:
            if trace["kind"] == "PreToolUse":
                tools.append(trace["payload"]["name"])
        if tools:
            sequences.append(tools)
    return sequences


def find_common_subsequences(sequences: List[List[str]], min_length=2, max_length=5):
    """Find the most common tool call subsequences."""
    subseqs = Counter()
    for seq in sequences:
        for i in range(len(seq)):
            for j in range(i + min_length, min(i + max_length + 1, len(seq) + 1)):
                subseqs[tuple(seq[i:j])] += 1
    return subseqs.most_common(20)


def extract_decision_points(sessions: Dict) -> List[Dict]:
    """Extract decision trees: if (result contains X) then call Y."""
    decisions = []
    for session_id, traces in sessions.items():
        prev_result = None
        prev_tool = None
        for trace in traces:
            if trace["kind"] == "PostToolUse":
                prev_result = trace["payload"].get("result", "")[:100]
                prev_tool = trace["payload"].get("name", "")
            elif trace["kind"] == "PreToolUse" and prev_result and prev_tool:
                decisions.append({
                    "if_tool": prev_tool,
                    "result_snippet": prev_result,
                    "then_tool": trace["payload"]["name"],
                    "then_args": trace["payload"]["args"],
                })
    return decisions


def extract_tool_invocation_stats(traces: List[Dict]) -> Dict[str, Dict]:
    """Collect tool usage statistics."""
    stats = defaultdict(lambda: {"count": 0, "examples": [], "errors": 0})
    for trace in traces:
        if trace["kind"] == "PreToolUse":
            tool_name = trace["payload"]["name"]
            args = trace["payload"]["args"]
            stats[tool_name]["count"] += 1
            if len(stats[tool_name]["examples"]) < 3:
                stats[tool_name]["examples"].append(args)
        elif trace["kind"] == "PostToolUseFailure":
            tool_name = trace["payload"]["name"]
            stats[tool_name]["errors"] += 1
    return stats


def report_markdown(sequences, subseqs, decisions, stats):
    """Generate a Markdown report."""
    lines = [
        "# Trace Analysis Report\n",
        f"Total sequences: {len(sequences)}\n",
    ]

    # Tool call sequences
    lines.append("## Most Common Tool Sequences\n")
    for seq, count in subseqs:
        pct = 100 * count / len(sequences)
        tools_str = " → ".join(seq)
        lines.append(f"- `{tools_str}`: {count} times ({pct:.1f}%)\n")

    # Tool statistics
    lines.append("\n## Tool Usage Statistics\n")
    for tool_name in sorted(stats.keys()):
        s = stats[tool_name]
        lines.append(f"\n### {tool_name}\n")
        lines.append(f"- Invocations: {s['count']}\n")
        lines.append(f"- Errors: {s['errors']}\n")
        if s["examples"]:
            lines.append(f"- Example args: `{s['examples'][0]}`\n")

    # Decision rules
    lines.append("\n## Decision Rules\n")
    decision_freqs = Counter((d["if_tool"], d["then_tool"]) for d in decisions)
    for (prev_tool, next_tool), count in decision_freqs.most_common(10):
        lines.append(f"- After `{prev_tool}` → `{next_tool}`: {count} times\n")

    return "".join(lines)


def main():
    parser = argparse.ArgumentParser(description="Analyze toroid traces")
    parser.add_argument("files", nargs="+", help="NDJSON trace files")
    parser.add_argument("--output", "-o", default=None, help="Output file (default: stdout)")
    parser.add_argument("--format", "-f", choices=["markdown", "json"], default="markdown")
    args = parser.parse_args()

    # Load all traces
    all_traces = []
    for file in args.files:
        try:
            all_traces.extend(load_traces(file))
        except FileNotFoundError:
            print(f"Error: {file} not found", file=sys.stderr)
            sys.exit(1)

    # Analyze
    sessions = extract_sessions(all_traces)
    sequences = extract_tool_sequences(sessions)
    subseqs = find_common_subsequences(sequences)
    decisions = extract_decision_points(sessions)
    stats = extract_tool_invocation_stats(all_traces)

    # Generate report
    if args.format == "markdown":
        report = report_markdown(sequences, subseqs, decisions, stats)
    else:  # json
        report = json.dumps({
            "sequences": sequences,
            "common_subsequences": [
                {"seq": list(seq), "count": count} for seq, count in subseqs
            ],
            "decision_points": decisions,
            "tool_stats": stats,
        }, indent=2)

    # Output
    if args.output:
        with open(args.output, "w") as f:
            f.write(report)
        print(f"Report written to {args.output}", file=sys.stderr)
    else:
        print(report)


if __name__ == "__main__":
    main()
