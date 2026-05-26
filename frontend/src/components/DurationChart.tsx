import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  Tooltip,
  ResponsiveContainer,
} from "recharts";
import { formatDuration } from "../lib/utils";

interface DurationPoint {
  build_id: string;
  timestamp: string;
  duration: number;
  passed: boolean;
}

interface DurationChartProps {
  history: DurationPoint[];
  testName?: string;
}

function formatDate(ts: string): string {
  const d = new Date(ts);
  return `${d.getMonth() + 1}/${d.getDate()}`;
}

interface DotProps {
  cx?: number;
  cy?: number;
  payload?: DurationPoint;
}

function CustomDot({ cx, cy, payload }: DotProps) {
  if (cx == null || cy == null || !payload) return null;
  return (
    <circle
      cx={cx}
      cy={cy}
      r={4}
      fill={payload.passed ? "#69f6b8" : "#ff716c"}
      stroke="none"
    />
  );
}

export function DurationChart({ history, testName }: DurationChartProps) {
  if (history.length === 0) return null;

  return (
    <div>
      {testName && (
        <h3 className="font-headline mb-2 text-sm font-semibold text-on-surface">
          Duration Trend
        </h3>
      )}
      <ResponsiveContainer width="100%" height={200}>
        <LineChart
          data={history}
          margin={{ top: 8, right: 16, bottom: 4, left: 8 }}
        >
          <XAxis
            dataKey="timestamp"
            tickFormatter={formatDate}
            tick={{ fill: "#adaaaa", fontSize: 11 }}
            stroke="#adaaaa"
            axisLine={false}
            tickLine={false}
          />
          <YAxis
            tickFormatter={(v: number) => formatDuration(v)}
            tick={{ fill: "#adaaaa", fontSize: 11 }}
            stroke="#adaaaa"
            axisLine={false}
            tickLine={false}
            width={48}
          />
          <Tooltip
            contentStyle={{
              background: "#0e0e0e",
              border: "1px solid #333",
              borderRadius: 8,
              fontSize: 12,
            }}
            labelFormatter={(label: unknown) => new Date(String(label)).toLocaleString()}
            formatter={(value: unknown) => [formatDuration(Number(value)), "Duration"]}
          />
          <Line
            type="monotone"
            dataKey="duration"
            stroke="#87adff"
            strokeWidth={2}
            dot={<CustomDot />}
            activeDot={{ r: 6, fill: "#87adff" }}
          />
        </LineChart>
      </ResponsiveContainer>
    </div>
  );
}
