-- +goose Up
ALTER TABLE image_health_reports ADD COLUMN nic_pci_ids TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE image_health_reports DROP COLUMN nic_pci_ids;
