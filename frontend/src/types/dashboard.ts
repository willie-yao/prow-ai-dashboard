// Types matching the Go backend JSON output

export interface RunSummary {
  build_id: string;
  passed: boolean;
  timestamp: string;
  duration_seconds?: number;
  tests_total?: number;
  tests_passed?: number;
  tests_failed?: number;
  tests_skipped?: number;
}

export interface JobSummary {
  name: string;
  tab_name: string;
  category: string;
  branch: string;
  description: string;
  minimum_interval: string;
  timeout: string;
  config_file: string;
  overall_status: "PASSING" | "FAILING" | "FLAKY";
  last_run: RunSummary | null;
  recent_runs: RunSummary[];
  pass_rate_7d: number;
  pass_rate_30d: number;
}

export interface Dashboard {
  generated_at: string;
  jobs: JobSummary[];
}

export interface TestCase {
  name: string;
  status: "passed" | "failed" | "skipped";
  duration_seconds: number;
  failure_message?: string;
  failure_body?: string;
  failure_location?: string;
  failure_location_url?: string;
  cluster_artifacts?: ClusterArtifacts;
  ai_summary?: AISummary;
  ai_analysis?: AIAnalysis;
}

export interface AISummary {
  generated_at: string;
  summary: string;
  is_transient: boolean;
}

export interface AIAnalysis {
  generated_at: string;
  model: string;
  root_cause: string;
  severity: string;
  suggested_fix: string;
  relevant_files?: string[];
}

export interface ClusterArtifacts {
  cluster_name: string;
  azure_activity_log?: string;
  machines?: MachineArtifacts[];
  pod_log_dirs?: Record<string, string>;
  bootstrap_resources_url?: string;
}

export interface MachineArtifacts {
  name: string;
  logs: Record<string, string>;
}

export interface BuildResult {
  build_id: string;
  job_name: string;
  started: string;
  finished: string;
  passed: boolean;
  result: string;
  duration_seconds: number;
  commit: string;
  repo_version?: string;
  prow_url: string;
  build_log_url: string;
  junit_url?: string;
  test_cases: TestCase[];
  tests_total: number;
  tests_passed: number;
  tests_failed: number;
  tests_skipped: number;
}

export interface TestFlakiness {
  test_name: string;
  job_name: string;
  total_runs: number;
  failures: number;
  passes: number;
  flip_rate: number;
  fail_rate: number;
  consecutive_failures: number;
  classification: "persistent" | "flaky" | "one-off";
  last_failure?: {
    build_id: string;
    timestamp: string;
    failure_message: string;
    error_hash: string;
  };
  first_failed_at?: string;
  error_patterns?: {
    normalized_message: string;
    error_hash: string;
    count: number;
    example_message: string;
  }[];
  duration_history?: {
    build_id: string;
    timestamp: string;
    duration: number;
    passed: boolean;
  }[];
}

export interface FlakinessReport {
  generated_at: string;
  most_flaky: TestFlakiness[];
  persistent_failures: TestFlakiness[];
  recently_broken: TestFlakiness[];
}

export interface JobDetail {
  name: string;
  runs: BuildResult[];
}

export interface SearchEntry {
  kind: "job" | "test";
  test_name: string;
  job_name: string;
  tab_name: string;
  branch: string;
  category: string;
  status: string;
  fail_rate: number;
}

export interface SearchIndex {
  generated_at: string;
  entries: SearchEntry[];
}
