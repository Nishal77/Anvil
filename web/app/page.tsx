"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import { createJob, getToken, setToken } from "@/lib/api";

export default function HomePage() {
  const router = useRouter();
  const [prompt, setPrompt] = useState("");
  const [token, setTokenInput] = useState(getToken());
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    setToken(token);
    setSubmitting(true);
    try {
      const job = await createJob(prompt);
      router.push(`/jobs/${job.id}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <main>
      <h1>Anvil</h1>
      <p>
        <Link href="/jobs">View all jobs &rarr;</Link>
      </p>
      <form onSubmit={handleSubmit}>
        <div>
          <label>
            Access token (from /auth/login or /auth/register)
            <br />
            <input
              type="text"
              value={token}
              onChange={(e) => setTokenInput(e.target.value)}
              style={{ width: "100%" }}
            />
          </label>
        </div>
        <div style={{ marginTop: "1rem" }}>
          <label>
            Prompt
            <br />
            <textarea
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
              rows={6}
              style={{ width: "100%" }}
              maxLength={8000}
            />
          </label>
        </div>
        <button type="submit" disabled={submitting || !prompt || !token}>
          {submitting ? "Submitting..." : "Submit"}
        </button>
      </form>
      {error && <pre style={{ color: "red" }}>{error}</pre>}
    </main>
  );
}
