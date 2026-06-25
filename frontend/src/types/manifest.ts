// Manifest mirrors backend/internal/project.Config (JSON form).
// The fetcher writes this to data/manifest.json on every run; the
// frontend loads it once at app boot via ManifestProvider.

export interface SourceRepo {
  owner: string;
  name: string;
}

export interface Branding {
  title: string;
  base_path: string;
  site_url: string;
  source_repo: SourceRepo;
}

export interface Source {
  include_presubmits?: boolean;
}

export interface TestGrid {
  dashboard: string;
}

export interface Storage {
  provider: string;
  bucket: string;
  base?: string;
  web_base?: string;
  prow_base?: string;
}

export interface CategoryRule {
  match: string;
  id: string;
  label: string;
}

export interface Manifest {
  id: string;
  name: string;
  short_name?: string;
  source: Source;
  testgrid: TestGrid;
  storage: Storage;
  branding: Branding;
  categories?: CategoryRule[];
  category_display_order?: string[];
  // Display-only hint derived at fetch time: the longest periodic-<x>-
  // prefix shared by a majority of discovered periodic jobs. Used by
  // shortJobName to strip boilerplate from job names in the UI.
  short_name_prefix?: string;
}
