-- Which bundles have been in force, and when.
--
-- Every decision and every audit record already carries a policy version — a
-- SHA-256 over the bundle that produced it. What was missing is the way back:
-- given that hash from a record six months old, the only route to the rules was
-- to check out every commit of the policy repository and rehash until one
-- matched. Theoretically fine, and nobody will do it.
--
-- ref and commit are attached afterwards by whatever published the bundle. The
-- server knows the version and the moment; it does not know the commit, because
-- a bundle may arrive as a directory, over a seam, or from somewhere with no
-- git in it at all.
CREATE TABLE IF NOT EXISTS policy_versions (
    tenant_id       TEXT        NOT NULL,

    -- "sha256:…", as computed at load. The identity, and it cannot disagree
    -- with the rules it names.
    version         TEXT        NOT NULL,

    -- Both ends, because either alone answers half the question. First says
    -- when a rule change took effect; last says whether it still is the rule
    -- change in effect.
    first_loaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_loaded_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Where the loader read it from, as a sentence rather than a path: a
    -- directory on one host means nothing on another.
    source          TEXT        NOT NULL DEFAULT '',

    ref             TEXT        NOT NULL DEFAULT '',
    commit_sha      TEXT        NOT NULL DEFAULT '',

    PRIMARY KEY (tenant_id, version)
);

CREATE INDEX IF NOT EXISTS policy_versions_recent_idx
    ON policy_versions (tenant_id, first_loaded_at DESC);
