ALTER TABLE platforms
ADD COLUMN passive_circuit_breaker_disabled BOOLEAN NOT NULL DEFAULT false;
