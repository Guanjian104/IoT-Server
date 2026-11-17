ALTER TABLE rtc_call
  ADD COLUMN callee_user_id BIGINT NULL,
  ADD COLUMN answered_by BIGINT NULL;
