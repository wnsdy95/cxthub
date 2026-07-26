-- Profile picture — Store the resized data URL (data:image/…;base64,…) as is from the client.
-- For small self-hosted environments, store directly in the column without additional blob infrastructure (server performs size and format validation).
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar TEXT;
