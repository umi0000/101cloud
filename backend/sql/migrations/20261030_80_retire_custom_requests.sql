-- Custom instances are provisioned directly by administrators; applications happen outside the platform.
DROP TABLE custom_instance_requests;
DROP TABLE residency_requests;
ALTER TABLE instances DROP COLUMN custom_reason;
