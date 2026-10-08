-- A connector may let each person sign in to the service with their own
-- account, so agents work with that person's permissions instead of one
-- shared account's.
ALTER TABLE connections ADD COLUMN per_user boolean NOT NULL DEFAULT false;

-- Each person's sign-in to such a connector.
CREATE TABLE accounts (
    connection_id text NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    user_id       text NOT NULL,
    -- Who they signed in as at the service, such as their Google email.
    account       text NOT NULL DEFAULT '',
    -- The refresh token, sealed under the connection and the person.
    token         bytea NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (connection_id, user_id)
);
CREATE INDEX accounts_user_idx ON accounts (user_id);
