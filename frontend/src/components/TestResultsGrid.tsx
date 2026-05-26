import { useMemo } from "react";
import { Link } from "react-router-dom";
import type { BuildResult } from "../types/dashboard";
import { shortTestName } from "../lib/utils";

interface TestResultsGridProps {
  runs: BuildResult[];
  jobName: string;
}

type CellStatus = "passed" | "failed" | "skipped" | "absent";

interface GridRow {
  testName: string;
  failCount: number;
  cells: CellStatus[];
}

const setupPatterns =
  /^(SynchronizedBeforeSuite|SynchronizedAfterSuite|BeforeSuite|AfterSuite)$/i;

function shortDate(dateStr: string): string {
  const d = new Date(dateStr);
  return `${d.getMonth() + 1}/${d.getDate()}`;
}

export function TestResultsGrid({ runs, jobName }: TestResultsGridProps) {
  // Sort runs oldest→newest (left to right)
  const sortedRuns = useMemo(
    () =>
      [...runs].sort(
        (a, b) =>
          new Date(a.started).getTime() - new Date(b.started).getTime(),
      ),
    [runs],
  );

  const gridRows = useMemo(() => {
    if (sortedRuns.length === 0) return [];

    // Build a map: testName → status per run index
    const testMap = new Map<string, CellStatus[]>();

    for (let col = 0; col < sortedRuns.length; col++) {
      const run = sortedRuns[col];
      for (const tc of run.test_cases) {
        if (!testMap.has(tc.name)) {
          testMap.set(tc.name, new Array(sortedRuns.length).fill("skipped"));
        }
        testMap.get(tc.name)![col] = tc.status;
      }
    }

    // Filter and build rows
    const rows: GridRow[] = [];

    for (const [testName, cells] of testMap) {
      const failCount = cells.filter((s) => s === "failed").length;
      const hasPass = cells.some((s) => s === "passed");
      const hasFail = failCount > 0;

      // Filter out skipped-only tests and setup/teardown (unless failed)
      if (!hasPass && !hasFail) continue;
      if (setupPatterns.test(testName) && !hasFail) continue;

      rows.push({ testName, failCount, cells });
    }

    // Sort: most failures first, then alphabetical
    rows.sort((a, b) => {
      if (b.failCount !== a.failCount) return b.failCount - a.failCount;
      return a.testName.localeCompare(b.testName);
    });

    return rows;
  }, [sortedRuns]);

  if (runs.length === 0 || gridRows.length === 0) {
    return (
      <div className="glass rounded-xl p-6 text-center">
        <p className="text-sm text-on-surface-variant">
          {runs.length === 0
            ? "No runs available."
            : "All tests passed across all runs — nothing to display."}
        </p>
      </div>
    );
  }

  return (
    <>
      {/* Mobile message */}
      <div className="md:hidden glass rounded-xl p-6 text-center">
        <p className="text-sm text-on-surface-variant">
          View on desktop for full test results grid
        </p>
      </div>
      {/* Desktop grid */}
      <div className="hidden md:block rounded-xl border border-outline-variant bg-surface">
      <div className="flex">
        {/* Test name column — fixed width, horizontally scrollable */}
        <div className="w-[300px] shrink-0 overflow-x-auto border-r border-outline-variant">
          <table className="border-collapse w-full">
            <thead>
              <tr className="h-8">
                <th className="bg-surface px-3 text-left font-label text-[10px] font-normal text-on-surface-variant whitespace-nowrap">
                  Test
                </th>
              </tr>
            </thead>
            <tbody>
              {gridRows.map((row) => (
                <tr key={row.testName} className="h-7 group hover:brightness-110">
                  <td className="bg-surface group-hover:brightness-110">
                    <Link
                      to={`/job/${encodeURIComponent(jobName)}/test/${encodeURIComponent(row.testName)}`}
                      className="block whitespace-nowrap px-3 text-xs text-on-surface transition-colors hover:text-primary"
                      title={row.testName}
                    >
                      {shortTestName(row.testName)}
                    </Link>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        {/* Results grid — scrollable horizontally */}
        <div className="overflow-x-auto">
          <table className="border-collapse">
            <thead>
              <tr className="h-8">
                {sortedRuns.map((run) => (
                  <th
                    key={run.build_id}
                    className="px-1 font-label text-[10px] font-normal text-on-surface-variant"
                  >
                    {shortDate(run.started)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {gridRows.map((row) => (
                <tr key={row.testName} className="h-7 group hover:brightness-110">
                  {row.cells.map((status, colIdx) => {
                    const run = sortedRuns[colIdx];
                    const cellColor =
                      status === "passed"
                        ? "bg-secondary"
                        : status === "failed"
                          ? "bg-error"
                          : "bg-on-surface-variant/30";

                    const cell = (
                      <div
                        className={`mx-auto h-5 w-12 rounded-[2px] ${cellColor}`}
                        title={`${shortTestName(row.testName)}\n#${run.build_id} — ${status}`}
                      />
                    );

                    return (
                      <td key={run.build_id} className="px-1 py-0.5">
                        {status !== "absent" ? (
                          <Link
                            to={`/job/${encodeURIComponent(jobName)}?run=${run.build_id}`}
                          >
                            {cell}
                          </Link>
                        ) : (
                          cell
                        )}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        </div>
      </div>
    </>
  );
}
