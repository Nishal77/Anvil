"use client";

import { useEffect, useRef, useState } from "react";
import { useParams } from "next/navigation";
import { API_BASE, getToken } from "@/lib/api";

// Every event_type the server can send (migration 005) — EventSource
// only delivers named events to a listener registered for that exact
// name, there's no wildcard.
const EVENT_TYPES = [
  "job_created",
  "plan_ready",
  "plan_approved",
  "step_started",
  "step_finished",
  "tool_call",
  "log_line",
  "stream_gap",
  "deploy_started",
  "deploy_ready",
  "job_finished",
  "error",
];

export default function JobPage() {
  const { id } = useParams<{ id: string }>();
  const [lines, setLines] = useState<string[]>([]);
  const [connected, setConnected] = useState(false);
  const logRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    const url = `${API_BASE}/v1/jobs/${id}/events?access_token=${encodeURIComponent(getToken())}`;
    const source = new EventSource(url);

    source.onopen = () => setConnected(true);
    source.onerror = () => setConnected(false);

    const append = (type: string) => (ev: MessageEvent) => {
      setLines((prev) => [...prev, `${type}: ${ev.data}`]);
    };
    for (const type of EVENT_TYPES) {
      source.addEventListener(type, append(type));
    }

    return () => source.close();
  }, [id]);

  useEffect(() => {
    logRef.current?.scrollTo(0, logRef.current.scrollHeight);
  }, [lines]);

  return (
    <main>
      <h1>Job {id}</h1>
      <p>{connected ? "connected" : "disconnected — reconnecting"}</p>
      <pre
        ref={logRef}
        style={{
          background: "#111",
          color: "#0f0",
          padding: "1rem",
          height: "70vh",
          overflowY: "auto",
          whiteSpace: "pre-wrap",
        }}
      >
        {lines.join("\n")}
      </pre>
    </main>
  );
}
