// The form generator.
//
// forge already decides, for the CLI, which parts of a schema flags can
// express and records the rest as NotExpressible with a pointer and a reason
// (internal/binding/flags.go). A form can express more than flags can --
// nested objects, arrays -- but not everything, and the same honesty applies:
// where a subtree cannot be drawn, say so and offer raw JSON for it rather
// than silently dropping the field.

import type { Schema, Violation } from "./api";

/** effectiveType reads the type, ignoring the "null" that jsonschema-go emits
 *  alongside it for a Go pointer or slice. Treating that union as a genuine
 *  one is what once made every optional field unreachable from the CLI. */
export function effectiveType(s: Schema): string {
  const t = s.type;
  if (Array.isArray(t)) {
    const real = t.filter((x) => x !== "null");
    return real.length === 1 ? String(real[0]) : "";
  }
  return t ?? "";
}

/** expressible reports whether a form can draw this subtree. */
export function expressible(s: Schema): boolean {
  if (s.oneOf || s.anyOf || s.allOf || s.prefixItems) return false;
  const t = effectiveType(s);
  if (t === "object") {
    return Object.values(s.properties ?? {}).every(expressible);
  }
  if (t === "array") {
    return !!s.items && effectiveType(s.items) !== "object" && expressible(s.items);
  }
  return t !== "";
}

type FieldProps = {
  name: string;
  schema: Schema;
  pointer: string;
  value: unknown;
  required: boolean;
  errors: Violation[];
  onChange: (pointer: string, value: unknown) => void;
};

function errorFor(pointer: string, errors: Violation[]): string | undefined {
  return errors.find((e) => e.pointer === pointer)?.message;
}

export function Field({ name, schema, pointer, value, required, errors, onChange }: FieldProps) {
  const type = effectiveType(schema);
  const problem = errorFor(pointer, errors);
  const label = (
    <label htmlFor={pointer}>
      {schema.title ?? name}
      {required && <span className="req" aria-label="required"> *</span>}
    </label>
  );

  let control: React.ReactNode;

  if (!expressible(schema)) {
    // The honest fallback. Raw JSON for this subtree, with the reason stated,
    // rather than a control that quietly loses half the value.
    control = (
      <>
        <textarea
          id={pointer}
          rows={4}
          defaultValue={value === undefined ? "" : JSON.stringify(value, null, 2)}
          onChange={(e) => {
            try {
              onChange(pointer, e.target.value === "" ? undefined : JSON.parse(e.target.value));
            } catch {
              /* keep the last valid value; the server will judge the rest */
            }
          }}
        />
        <p className="muted">this shape has no form control — give it as JSON</p>
      </>
    );
  } else if (schema.enum) {
    control = (
      <select
        id={pointer}
        value={value === undefined ? "" : String(value)}
        onChange={(e) => onChange(pointer, e.target.value === "" ? undefined : e.target.value)}
      >
        <option value="">—</option>
        {schema.enum.map((o) => (
          <option key={String(o)} value={String(o)}>
            {String(o)}
          </option>
        ))}
      </select>
    );
  } else if (type === "boolean") {
    control = (
      <input
        id={pointer}
        type="checkbox"
        checked={value === true}
        onChange={(e) => onChange(pointer, e.target.checked)}
      />
    );
  } else if (type === "integer" || type === "number") {
    control = (
      <input
        id={pointer}
        type="number"
        step={type === "integer" ? 1 : "any"}
        value={value === undefined ? "" : String(value)}
        onChange={(e) => onChange(pointer, e.target.value === "" ? undefined : Number(e.target.value))}
      />
    );
  } else if (type === "array") {
    const items = Array.isArray(value) ? (value as unknown[]) : [];
    control = (
      <div className="array">
        {items.map((item, i) => (
          <input
            key={i}
            value={String(item ?? "")}
            onChange={(e) => {
              const next = [...items];
              next[i] = e.target.value;
              onChange(pointer, next);
            }}
          />
        ))}
        <button type="button" onClick={() => onChange(pointer, [...items, ""])}>
          add
        </button>
      </div>
    );
  } else if (type === "object") {
    return (
      <fieldset>
        <legend>{schema.title ?? name}</legend>
        <Fields
          schema={schema}
          pointer={pointer}
          value={(value ?? {}) as Record<string, unknown>}
          errors={errors}
          onChange={onChange}
        />
      </fieldset>
    );
  } else {
    control = (
      <input
        id={pointer}
        value={value === undefined ? "" : String(value)}
        onChange={(e) => onChange(pointer, e.target.value === "" ? undefined : e.target.value)}
      />
    );
  }

  return (
    <div className={problem ? "field bad" : "field"}>
      {label}
      {control}
      {schema.description && <p className="muted">{schema.description}</p>}
      {problem && <p className="error">{problem}</p>}
    </div>
  );
}

export function Fields({
  schema,
  pointer,
  value,
  errors,
  onChange,
}: {
  schema: Schema;
  pointer: string;
  value: Record<string, unknown>;
  errors: Violation[];
  onChange: (pointer: string, value: unknown) => void;
}) {
  const props = schema.properties ?? {};
  const required = new Set(schema.required ?? []);
  return (
    <>
      {Object.entries(props).map(([name, sub]) => (
        <Field
          key={name}
          name={name}
          schema={sub}
          // RFC 6901: "/" and "~" are escaped, and forge's violations use the
          // same encoding, so the two sides line up exactly.
          pointer={`${pointer}/${name.replace(/~/g, "~0").replace(/\//g, "~1")}`}
          value={value[name]}
          required={required.has(name)}
          errors={errors}
          onChange={onChange}
        />
      ))}
    </>
  );
}

/** setAt writes a value at a JSON Pointer, creating objects on the way. */
export function setAt(root: Record<string, unknown>, pointer: string, value: unknown) {
  const parts = pointer.split("/").slice(1).map((p) => p.replace(/~1/g, "/").replace(/~0/g, "~"));
  let node: Record<string, unknown> = root;
  for (let i = 0; i < parts.length - 1; i++) {
    const key = parts[i]!;
    if (typeof node[key] !== "object" || node[key] === null) node[key] = {};
    node = node[key] as Record<string, unknown>;
  }
  const last = parts[parts.length - 1]!;
  if (value === undefined) delete node[last];
  else node[last] = value;
}
