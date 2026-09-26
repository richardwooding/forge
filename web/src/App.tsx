import { useEffect, useMemo, useState } from "react";
import {
  describeTool,
  invoke,
  listTools,
  type Problem,
  type Result,
  type ToolDetail,
  type ToolSummary,
  type Violation,
} from "./api";
import { Fields, setAt } from "./form";

/** badge mirrors capability.Kind.Badge() so a capability looks the same here
 *  as it does in the terminal. */
const badges: Record<string, string> = {
  "fs.read": "📁",
  "fs.write": "📝",
  "net.http": "🌐",
  env: "🔧",
  secret: "🔑",
  "clock.wall": "🕐",
  kv: "🗄",
  "tool.invoke": "🔗",
};

/** chipColour mirrors internal/ui's FNV-1a hash so a label is the same colour
 *  in the browser as in the terminal, with no configuration on either side. */
const chipColours = ["#a78bfa", "#22d3ee", "#34d399", "#60a5fa", "#e879f9", "#2dd4bf"];
function chipColour(label: string): string {
  let h = 2166136261;
  for (let i = 0; i < label.length; i++) {
    h ^= label.charCodeAt(i);
    h = Math.imul(h, 16777619) >>> 0;
  }
  return chipColours[h % chipColours.length]!;
}

export default function App() {
  const [tools, setTools] = useState<ToolSummary[]>([]);
  const [selected, setSelected] = useState<string>();
  const [detail, setDetail] = useState<ToolDetail>();
  const [input, setInput] = useState<Record<string, unknown>>({});
  const [violations, setViolations] = useState<Violation[]>([]);
  const [result, setResult] = useState<Result>();
  const [failure, setFailure] = useState<Problem>();
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    listTools().then(setTools).catch(setFailure);
  }, []);

  useEffect(() => {
    if (!selected) return;
    setDetail(undefined);
    setInput({});
    setViolations([]);
    setResult(undefined);
    setFailure(undefined);
    describeTool(selected).then(setDetail).catch(setFailure);
  }, [selected]);

  const needs = useMemo(() => detail?.requires ?? [], [detail]);

  // The one combination no sandbox can make safe, and the same warning the
  // terminal prompt gives: a tool that can read secrets and reach the network
  // can send one to the other.
  const exfiltration =
    needs.some((r) => r.kind === "secret") && needs.some((r) => r.kind === "net.http");

  async function run(e: React.FormEvent) {
    e.preventDefault();
    if (!detail) return;
    setBusy(true);
    setViolations([]);
    setFailure(undefined);
    setResult(undefined);
    try {
      setResult(await invoke(detail.name, input));
    } catch (err) {
      const p = err as Problem;
      setFailure(p);
      // Each violation carries a JSON Pointer at the field that was wrong, so
      // the error lands on the input rather than in a wall of text. This is
      // why the GUI does not validate: the server already did, identically to
      // every other surface.
      setViolations(p.violations ?? []);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="app">
      <aside>
        <h1>forge</h1>
        <ul className="tools">
          {tools.map((t) => (
            <li key={t.name}>
              <button
                className={t.name === selected ? "on" : undefined}
                onClick={() => setSelected(t.name)}
              >
                <span className="name">{t.name}</span>
                <span className="chips">
                  {(t.labels ?? []).map((l) => (
                    <span key={l} className="chip" style={{ borderColor: chipColour(l), color: chipColour(l) }}>
                      {l}
                    </span>
                  ))}
                </span>
                <span className="wants">{(t.requires ?? []).map((k) => badges[k] ?? "?").join("")}</span>
              </button>
            </li>
          ))}
        </ul>
      </aside>

      <main>
        {!detail && <p className="muted">Choose a tool.</p>}

        {detail && (
          <>
            <h2>{detail.name}</h2>
            {detail.summary && <p>{detail.summary}</p>}

            {needs.length > 0 && (
              <section className="needs">
                <h3>wants</h3>
                {needs.map((r) => (
                  <p key={r.kind}>
                    <span aria-hidden="true">{badges[r.kind] ?? "?"}</span> <code>{r.kind}</code>{" "}
                    {(r.scope ?? []).join(", ")}
                    {r.reason && <span className="muted"> — {r.reason}</span>}
                  </p>
                ))}
                {exfiltration && (
                  <p className="warn">
                    This tool can read your secrets and reach the network. Nothing stops it
                    sending one to the other — that is what both capabilities are for.
                  </p>
                )}
              </section>
            )}

            <form onSubmit={run}>
              <Fields
                schema={detail.inputSchema}
                pointer=""
                value={input}
                errors={violations}
                onChange={(pointer, value) => {
                  setInput((prev) => {
                    const next = structuredClone(prev);
                    setAt(next, pointer, value);
                    return next;
                  });
                }}
              />
              <button type="submit" disabled={busy}>
                {busy ? "running…" : "Run"}
              </button>
            </form>

            {failure && (
              <section className="failure">
                <h3>{failure.title ?? "that did not work"}</h3>
                {failure.detail && <p>{failure.detail}</p>}
              </section>
            )}

            {result && <Output result={result} />}
          </>
        )}
      </main>
    </div>
  );
}

function Output({ result }: { result: Result }) {
  return (
    <section className="result">
      <h3>result</h3>
      {result.truncated && (
        <p className="warn">forge cut this at its size limit — it is not the whole answer.</p>
      )}
      {result.kind === "json" && <pre>{JSON.stringify(result.value, null, 2)}</pre>}
      {result.kind === "text" && <pre>{result.value}</pre>}
      {result.kind === "bytes" && (
        // Never rendered inline. A tool's bytes are its own; the browser
        // should not be persuaded to interpret them as a document.
        <a download="output" href={URL.createObjectURL(result.blob)}>
          download {result.mediaType || "output"} ({result.blob.size} bytes)
        </a>
      )}
    </section>
  );
}
