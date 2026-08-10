import { useState } from "react";
import {
  useTable,
  tableFeatures,
  createColumnHelper as tanstackCreateColumnHelper,
  createCoreRowModel,
  createSortedRowModel,
  createFilteredRowModel,
  rowSortingFeature,
  columnFilteringFeature,
  globalFilteringFeature,
  columnVisibilityFeature,
  flexRender,
  type ColumnDef,
  type RowData,
  type SortingState,
} from "@tanstack/react-table";

// react-table v9 is feature-based: a table only carries the features it
// declares, and the `features` object also holds the row-model factories. The
// whole app's tables use the same set — global text filtering, click-to-sort,
// column visibility for rendering — so it is declared once here and threaded
// through both the column helper and the table instance so their generics line
// up.
export const dataTableFeatures = tableFeatures({
  rowSortingFeature,
  columnFilteringFeature,
  globalFilteringFeature,
  columnVisibilityFeature,
  coreRowModel: createCoreRowModel(),
  sortedRowModel: createSortedRowModel(),
  filteredRowModel: createFilteredRowModel(),
});
export type DataTableFeatures = typeof dataTableFeatures;

// createColumnHelper bound to the app feature set, so pages keep writing
// createColumnHelper<Row>() with a single type argument instead of repeating
// the feature generic at every call site.
export function createColumnHelper<TData extends RowData>() {
  return tanstackCreateColumnHelper<DataTableFeatures, TData>();
}

// The final `any` is `ColumnDef`'s cell-value type parameter. A column array is
// heterogeneous — each accessor yields a different value type — so an aggregate
// column list cannot name a single value type; `any` here mirrors how TanStack
// Table itself types a mixed ColumnDef list.
export type DataTableColumn<T extends RowData> = ColumnDef<DataTableFeatures, T, any>;

export interface DataTableProps<T extends RowData> {
  data: T[];
  columns: DataTableColumn<T>[];
  filterPlaceholder?: string | undefined;
  onRowClick?: ((row: T) => void) | undefined;
  /** Optional empty-state body when no rows match. */
  emptyMessage?: string | undefined;
}

/**
 * Operator-grade data table. Dense, monospace, sticky headers,
 * subtle row striping, no shadows, sharp corners. Filter input lives
 * inside the table chrome — single composed unit, no floating bar.
 * Hover background pulls the per-app accent in lightly so the row
 * announces itself without screaming.
 */
export function DataTable<T extends RowData>({
  data,
  columns,
  filterPlaceholder,
  onRowClick,
  emptyMessage = "No rows match.",
}: DataTableProps<T>) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [globalFilter, setGlobalFilter] = useState("");

  const table = useTable<DataTableFeatures, T>({
    features: dataTableFeatures,
    data,
    columns,
    state: { sorting, globalFilter },
    onSortingChange: setSorting,
    onGlobalFilterChange: setGlobalFilter,
  });

  const rows = table.getRowModel().rows;

  return (
    <div
      style={{
        background: "var(--color-surface)",
        border: "1px solid var(--color-border)",
        borderRadius: "var(--radius-sm)",
      }}
    >
      <style>{`
        .dt-row { transition: background-color 0.1s var(--ease-out-quint); }
        .dt-row[data-row-stripe="even"] { background: var(--color-surface); }
        .dt-row[data-row-stripe="odd"]  { background: var(--color-bg-subtle); }
        .dt-row[data-clickable="true"]:hover,
        .dt-row[data-clickable="true"]:focus-visible {
          background: color-mix(in oklch, var(--color-accent-soft) 55%, var(--color-surface));
        }
      `}</style>

      <div
        className="flex items-center justify-between gap-4 px-3 py-2"
        style={{ borderBottom: "1px solid var(--color-border)" }}
      >
        <input
          type="text"
          value={globalFilter}
          onChange={(e) => setGlobalFilter(e.target.value)}
          placeholder={filterPlaceholder ?? "Search…"}
          aria-label="Filter table"
          className="w-full max-w-sm"
          style={{
            background: "transparent",
            border: "none",
            padding: "0.25rem 0",
            fontSize: "0.78rem",
          }}
        />
        <div
          className="font-mono text-[10px] uppercase tracking-[0.18em]"
          style={{ color: "var(--color-fg-subtle)" }}
        >
          {rows.length} / {data.length} rows
        </div>
      </div>

      <div className="overflow-x-auto">
        <table
          className="min-w-full font-mono"
          style={{
            fontSize: "0.78rem",
            borderCollapse: "separate",
            borderSpacing: 0,
          }}
        >
          <thead style={{ background: "var(--color-bg-subtle)" }}>
            {table.getHeaderGroups().map((hg) => (
              <tr key={hg.id}>
                {hg.headers.map((header, idx) => {
                  const sort = header.column.getIsSorted();
                  const canSort = header.column.getCanSort();
                  const ariaSort: React.AriaAttributes["aria-sort"] =
                    sort === "asc" ? "ascending" : sort === "desc" ? "descending" : canSort ? "none" : undefined;
                  const isLastHeader = idx === hg.headers.length - 1;
                  return (
                    <th
                      key={header.id}
                      scope="col"
                      aria-sort={ariaSort}
                      className="select-none text-left uppercase tracking-[0.15em]"
                      style={{
                        padding: "0.5rem 0.75rem",
                        fontSize: "0.62rem",
                        fontWeight: 500,
                        color: sort
                          ? "var(--color-accent)"
                          : "var(--color-fg-subtle)",
                        borderBottom: "1px solid var(--color-border)",
                        borderRight: isLastHeader
                          ? undefined
                          : "1px solid color-mix(in oklch, var(--color-border) 35%, transparent)",
                        whiteSpace: "nowrap",
                      }}
                    >
                      {header.isPlaceholder ? null : canSort ? (
                        <button
                          type="button"
                          onClick={header.column.getToggleSortingHandler()}
                          className="inline-flex items-center gap-1"
                          style={{
                            background: "transparent",
                            border: "none",
                            padding: 0,
                            color: "inherit",
                            font: "inherit",
                            letterSpacing: "inherit",
                            textTransform: "inherit",
                            cursor: "pointer",
                          }}
                        >
                          {flexRender(header.column.columnDef.header, header.getContext())}
                          <span
                            aria-hidden
                            style={{ opacity: sort ? 1 : 0.25, fontSize: "0.7em" }}
                          >
                            {sort === "asc" ? "↑" : sort === "desc" ? "↓" : "↕"}
                          </span>
                        </button>
                      ) : (
                        <span className="inline-flex items-center gap-1">
                          {flexRender(header.column.columnDef.header, header.getContext())}
                        </span>
                      )}
                    </th>
                  );
                })}
              </tr>
            ))}
          </thead>
          <tbody>
            {rows.length === 0 ? (
              <tr>
                <td
                  colSpan={columns.length}
                  className="text-center font-mono uppercase tracking-[0.2em]"
                  style={{
                    padding: "2rem 0.75rem",
                    fontSize: "0.7rem",
                    color: "var(--color-fg-subtle)",
                  }}
                >
                  {emptyMessage}
                </td>
              </tr>
            ) : (
              rows.map((row, i) => (
                <tr
                  key={row.id}
                  onClick={onRowClick ? () => onRowClick(row.original) : undefined}
                  onKeyDown={
                    onRowClick
                      ? (e) => {
                          if (e.key === "Enter" || e.key === " ") {
                            e.preventDefault();
                            onRowClick(row.original);
                          }
                        }
                      : undefined
                  }
                  tabIndex={onRowClick ? 0 : undefined}
                  role={onRowClick ? "button" : undefined}
                  aria-label={onRowClick ? "Open row detail" : undefined}
                  data-row-index={i}
                  data-clickable={onRowClick ? "true" : undefined}
                  data-row-stripe={i % 2 === 0 ? "even" : "odd"}
                  className="dt-row reveal"
                  style={{
                    cursor: onRowClick ? "pointer" : undefined,
                    "--reveal-delay": `${Math.min(i * 16, 240)}ms`,
                  } as React.CSSProperties}
                >
                  {row.getVisibleCells().map((cell, idx) => {
                    const isLastCell = idx === row.getVisibleCells().length - 1;
                    return (
                      <td
                        key={cell.id}
                        style={{
                          padding: "0.375rem 0.75rem",
                          borderBottom:
                            "1px solid color-mix(in oklch, var(--color-border) 60%, transparent)",
                          borderRight: isLastCell
                            ? undefined
                            : "1px solid color-mix(in oklch, var(--color-border) 35%, transparent)",
                          whiteSpace: "nowrap",
                          color: "var(--color-fg)",
                        }}
                      >
                        {flexRender(cell.column.columnDef.cell, cell.getContext())}
                      </td>
                    );
                  })}
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
