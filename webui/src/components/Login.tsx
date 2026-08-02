import { useState } from "react";
import { setStatsKey } from "../api";

export function LoginView({ onAuthed }: { onAuthed: () => void }) {
  const [key, setKey] = useState("");

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = key.trim();
    if (!trimmed) return;
    setStatsKey(trimmed);
    onAuthed();
  };

  return (
    <div className="login-wrap">
      <form className="login-card" onSubmit={submit}>
        <div className="login-logo">apid</div>
        <p className="login-title">Stats dashboard is locked</p>
        <p className="login-hint">
          Enter the <code>stats_api_key</code> configured on the gateway to view metrics.
        </p>
        <input
          type="password"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          placeholder="stats API key"
          autoFocus
          autoComplete="current-password"
        />
        <button type="submit" disabled={!key.trim()}>Unlock</button>
      </form>
    </div>
  );
}
