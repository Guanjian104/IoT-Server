ALTER TABLE device_command DROP CHECK chk_command_state;

ALTER TABLE device_command ADD CONSTRAINT chk_command_state CHECK (state IN ('pending', 'sending', 'sent', 'accepted', 'failed', 'timeout', 'uncertain', 'needs_attention', 'cancelled'));
