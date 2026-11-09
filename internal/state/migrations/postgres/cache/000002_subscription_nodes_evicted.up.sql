ALTER TABLE subscription_nodes
ADD COLUMN evicted BOOLEAN NOT NULL DEFAULT false;
