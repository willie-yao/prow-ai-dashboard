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
  test_infra_paths: string[];
  file_prefix?: string;
}

export interface TestGrid {
  dashboard: string;
}

export interface GCS {
  bucket: string;
}

export interface CAPI {
  cluster_name_prefix: string;
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
  gcs: GCS;
  branding: Branding;
  categories?: CategoryRule[];
  category_display_order?: string[];
  capi?: CAPI;
}
