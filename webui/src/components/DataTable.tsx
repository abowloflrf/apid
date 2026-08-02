import { useMemo, type ReactNode } from "react";

export interface Column<T> {
  key: string;
  label: string;
  text?: boolean;
  cls?: string;
  sortable?: boolean;
  sortVal?: (r: T) => number | string;
  render: (r: T) => ReactNode;
}

export interface SortState {
  key: string;
  dir: "asc" | "desc";
}

interface Props<T> {
  columns: Column<T>[];
  rows: T[];
  sort: SortState;
  onSort: (key: string) => void;
  onRowClick?: (row: T) => void;
}

export function DataTable<T>({ columns, rows, sort, onSort, onRowClick }: Props<T>) {
  const sorted = useMemo(() => {
    const out = [...rows];
    if (sort.key) {
      const col = columns.find((c) => c.key === sort.key);
      const get = (r: T): number | string =>
        col && col.sortVal ? col.sortVal(r) : ((r as Record<string, unknown>)[sort.key] as number | string);
      out.sort((a, b) => {
        const x = get(a), y = get(b);
        if (typeof x === "number" && typeof y === "number") return sort.dir === "asc" ? x - y : y - x;
        return sort.dir === "asc" ? String(x).localeCompare(String(y)) : String(y).localeCompare(String(x));
      });
    }
    return out;
  }, [rows, sort, columns]);

  return (
    <table className="data-table">
      <thead>
        <tr>
          {columns.map((c) => (
            <th
              key={c.key}
              className={c.text ? "text" : ""}
              onClick={c.sortable === false ? undefined : () => onSort(c.key)}
            >
              {c.label}
              {c.key === sort.key ? <span className="arrow">{sort.dir === "asc" ? "▲" : "▼"}</span> : null}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {!sorted.length ? (
          <tr className="empty-row">
            <td colSpan={columns.length}>No data for this filter.</td>
          </tr>
        ) : (
          sorted.map((r, i) => (
            <tr
              key={i}
              className={onRowClick ? "clickable" : undefined}
              onClick={
                onRowClick
                  ? () => {
                      // Don't hijack a click the user made to select cell text.
                      if (window.getSelection && String(window.getSelection())) return;
                      onRowClick(r);
                    }
                  : undefined
              }
            >
              {columns.map((c) => (
                <td key={c.key} className={c.text ? "text" : c.cls || ""}>{c.render(r)}</td>
              ))}
            </tr>
          ))
        )}
      </tbody>
    </table>
  );
}
