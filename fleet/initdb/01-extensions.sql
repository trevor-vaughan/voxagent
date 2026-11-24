-- OpenVoxDB refuses to start without pg_trgm: it logs
--   "PuppetDB requires the PostgreSQL `pg_trgm` extension"
-- and exits 0 after about six seconds. The clean exit code makes this
-- look like a successful run, so create the extension up front.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
