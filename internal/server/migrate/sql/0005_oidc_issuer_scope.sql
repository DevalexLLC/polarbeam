-- OIDC subjects are unique only within an issuer (OIDC Core 5.7). Keying
-- federated identities on the bare subject let a re-pointed oidc_settings
-- row merge unrelated accounts that happened to share a sub — and live
-- sessions from the old provider would inherit the new identity's role.
-- Scope identities to (issuer, subject).

ALTER TABLE users ADD COLUMN oidc_issuer text;

-- Attribute existing federated rows to the currently configured issuer —
-- the best available guess. An empty issuer (OIDC configured then cleared)
-- leaves the row permanently unreachable, which is the safe direction: no
-- verified ID token ever carries an empty iss.
UPDATE users
   SET oidc_issuer = (SELECT issuer FROM oidc_settings WHERE id)
 WHERE auth_source = 'oidc';

ALTER TABLE users DROP CONSTRAINT users_auth_shape;
ALTER TABLE users ADD CONSTRAINT users_auth_shape CHECK (
    (auth_source = 'local' AND password_hash IS NOT NULL
        AND oidc_subject IS NULL AND oidc_issuer IS NULL)
 OR (auth_source = 'oidc'  AND password_hash IS NULL
        AND oidc_subject IS NOT NULL AND oidc_issuer IS NOT NULL)
);

DROP INDEX users_oidc_subject_idx;
CREATE UNIQUE INDEX users_oidc_identity_idx
    ON users (oidc_issuer, oidc_subject) WHERE oidc_subject IS NOT NULL;

-- Pre-upgrade SSO sessions may have been issued under a different issuer
-- than the attribution above; sign them out. SSO users just log in again.
-- Local (break-glass) sessions are untouched.
DELETE FROM sessions USING users
 WHERE sessions.user_id = users.id AND users.auth_source = 'oidc';
