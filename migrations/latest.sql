-- Session/security schema for the session-intelligence engine (single-tenant).
-- The 9 tables the engine and reference readers use, with row-level security
-- policies and cross-table foreign keys removed.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE public.harness_artifact_classifications (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    organization_id uuid NOT NULL,
    task text NOT NULL,
    harness_artifact_id uuid NOT NULL,
    verdicts_by_version jsonb DEFAULT '{}'::jsonb NOT NULL,
    override_verdict jsonb,
    override_set_by_person_id uuid,
    override_reason text,
    attempting_version text,
    attempting_digest text,
    attempt_state text NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    lease_expires_at timestamp with time zone,
    lease_generation bigint DEFAULT 0 NOT NULL,
    intent_class text,
    intent_class_source text,
    intent_classifier_version integer,
    intent_classified_from_input_hash text,
    intent_classified_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT harness_artifact_classifications_attempt_paired CHECK (((attempt_state = 'HARNESS_CLASSIFICATION_ATTEMPT_STATE_IN_FLIGHT'::text) = ((attempting_version IS NOT NULL) AND (attempting_digest IS NOT NULL) AND (lease_expires_at IS NOT NULL)))),
    CONSTRAINT harness_artifact_classifications_override_paired CHECK (((override_verdict IS NOT NULL) = (override_set_by_person_id IS NOT NULL)))
);
CREATE TABLE public.harness_artifacts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    organization_id uuid NOT NULL,
    person_id uuid,
    ide_type text NOT NULL,
    scope text NOT NULL,
    type text NOT NULL,
    name text NOT NULL,
    repository_id uuid,
    source_path text NOT NULL,
    current_file_id uuid NOT NULL,
    baseline_file_id uuid,
    deviation_state text,
    operational_state text DEFAULT 'HARNESS_OPERATIONAL_STATE_ENABLED'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_synced_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    hook_config jsonb,
    artifact_source text DEFAULT 'HARNESS_ARTIFACT_SOURCE_DESKTOP'::text NOT NULL,
    branch text,
    files jsonb DEFAULT '{}'::jsonb NOT NULL,
    content_generation bigint DEFAULT 0 NOT NULL,
    loop_type text DEFAULT 'LOOP_TYPE_UNSPECIFIED'::text NOT NULL,
    loop_type_source text DEFAULT 'LOOP_TYPE_SOURCE_UNSPECIFIED'::text NOT NULL,
    CONSTRAINT harness_artifacts_hook_config_chk CHECK (((hook_config IS NULL) OR (type = 'HARNESS_ARTIFACT_TYPE_HOOK'::text))),
    CONSTRAINT harness_artifacts_loop_type_source_agree CHECK ((((loop_type = 'LOOP_TYPE_UNSPECIFIED'::text) AND (loop_type_source = 'LOOP_TYPE_SOURCE_UNSPECIFIED'::text)) OR ((loop_type <> 'LOOP_TYPE_UNSPECIFIED'::text) AND (loop_type_source <> 'LOOP_TYPE_SOURCE_UNSPECIFIED'::text)))),
    CONSTRAINT harness_artifacts_person_by_source_chk CHECK ((((artifact_source = 'HARNESS_ARTIFACT_SOURCE_VCS'::text) AND (person_id IS NULL)) OR ((artifact_source <> 'HARNESS_ARTIFACT_SOURCE_VCS'::text) AND (person_id IS NOT NULL)))),
    CONSTRAINT harness_artifacts_scope_repo_chk CHECK ((((scope = 'HARNESS_ARTIFACT_SCOPE_USER_GLOBAL'::text) AND (repository_id IS NULL)) OR ((scope = ANY (ARRAY['HARNESS_ARTIFACT_SCOPE_REPO_LOCAL'::text, 'HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED'::text])) AND (repository_id IS NOT NULL))))
);
CREATE TABLE public.raw_records (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    organization_id uuid NOT NULL,
    connector_id character varying(255) NOT NULL,
    operation_id character varying(255) NOT NULL,
    data_date timestamp with time zone NOT NULL,
    record_key text NOT NULL,
    payload jsonb NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb,
    processing_status character varying(32) DEFAULT 'pending'::character varying NOT NULL,
    retry_count integer DEFAULT 0 NOT NULL,
    parse_error text,
    sync_run_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
)
PARTITION BY RANGE (data_date);
CREATE TABLE public.security_flags (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    organization_id uuid NOT NULL,
    person_id uuid NOT NULL,
    raw_record_id uuid NOT NULL,
    flag_type text NOT NULL,
    severity text DEFAULT 'medium'::text NOT NULL,
    flag_details jsonb,
    status text DEFAULT 'open'::text NOT NULL,
    resolved_by uuid,
    resolved_at timestamp with time zone,
    flag_date date NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    metadata jsonb DEFAULT '{}'::jsonb
);
CREATE TABLE public.session_analyses (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    organization_id uuid NOT NULL,
    person_id uuid NOT NULL,
    raw_record_id uuid NOT NULL,
    conversation_id text,
    session_id text,
    ide_type text,
    conversation_started_at timestamp with time zone NOT NULL,
    last_active_at timestamp with time zone,
    vocabulary_version integer NOT NULL,
    cap_token_split text,
    cap_tool_outcome text,
    cap_reasoning_trace text,
    cap_assistant_text text,
    session_steering text,
    goal_achieved text,
    smoothness text,
    friction_cause text,
    speed text,
    token_efficiency text,
    goal_is_coding_work boolean,
    underlying_goal text,
    score_overall smallint,
    score_clarity smallint,
    score_specificity smallint,
    score_context_provision smallint,
    score_actionability smallint,
    score_completeness smallint,
    score_efficiency smallint,
    weighted_score numeric,
    estimated_cost_usd numeric,
    own_estimated_cost_usd numeric,
    total_prompts integer,
    total_tokens bigint,
    wasted_tokens bigint,
    token_breakdown jsonb,
    subagent_usage jsonb,
    iteration_count integer,
    effective_active_duration_s integer,
    effective_ai_code_percent numeric,
    effective_context_usage_percent numeric,
    mcp_server_count integer,
    delegation_level text,
    effectiveness_source text,
    is_session_effective boolean,
    session_outcome text,
    user_interruptions integer,
    plan_tier text,
    plan_tier_source text,
    plan_tier_origin text,
    seat_monthly_price_usd numeric,
    has_extra_usage_enabled boolean,
    account_uuid text,
    login_email text,
    repo_name text,
    project_name text,
    session_duration_seconds integer,
    cache_hit_rate_percent numeric,
    compaction_count integer,
    files_edited_count integer,
    first_user_msg_ts timestamp with time zone,
    turn_count integer,
    human_turn_count integer,
    delegated_turn_count integer,
    phase_count integer,
    tool_call_count integer,
    active_share numeric,
    work_type text,
    segmentation_suspect boolean,
    languages jsonb,
    models_used jsonb,
    friction_counts jsonb,
    invoked_skills jsonb,
    artifact_adjudications jsonb,
    harness_findings jsonb,
    discarded_ranges jsonb,
    signal_coverage jsonb,
    session_meta_raw jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    judged_at timestamp with time zone,
    judged_disposition text,
    superseded_at timestamp with time zone,
    skill_assessments jsonb,
    discard_disposition text,
    evidence_artifact_disposition text,
    evidence_roundtrip_disposition text,
    read_security_issues jsonb,
    mcp_server_names jsonb GENERATED ALWAYS AS (
CASE
    WHEN (jsonb_typeof((session_meta_raw -> 'mcpServerNames'::text)) = 'array'::text) THEN jsonb_set('{"values": []}'::jsonb, '{values}'::text[], (session_meta_raw -> 'mcpServerNames'::text))
    ELSE NULL::jsonb
END) STORED,
    tool_errors text GENERATED ALWAYS AS ((session_meta_raw ->> 'toolErrors'::text)) STORED,
    session_type text GENERATED ALWAYS AS ((session_meta_raw ->> 'sessionType'::text)) STORED,
    cost_is_estimated boolean GENERATED ALWAYS AS (
CASE
    WHEN ((session_meta_raw ->> 'costIsEstimated'::text) = ANY (ARRAY['true'::text, 'false'::text])) THEN ((session_meta_raw ->> 'costIsEstimated'::text))::boolean
    ELSE NULL::boolean
END) STORED,
    cost_is_input_only boolean GENERATED ALWAYS AS (
CASE
    WHEN ((session_meta_raw ->> 'costIsInputOnly'::text) = ANY (ARRAY['true'::text, 'false'::text])) THEN ((session_meta_raw ->> 'costIsInputOnly'::text))::boolean
    ELSE NULL::boolean
END) STORED,
    user_message_timestamps jsonb GENERATED ALWAYS AS (
CASE
    WHEN (jsonb_typeof((session_meta_raw -> 'userMessageTimestamps'::text)) = 'array'::text) THEN jsonb_set('{"values": []}'::jsonb, '{values}'::text[], (session_meta_raw -> 'userMessageTimestamps'::text))
    ELSE NULL::jsonb
END) STORED,
    taxonomy_issues jsonb,
    waste_categories jsonb,
    ai_reviewed boolean,
    improved_prompt text,
    recommendations jsonb,
    task_type text,
    origin text DEFAULT 'SESSION_ANALYSIS_ORIGIN_LIVE'::text
);
CREATE TABLE public.session_analysis_state (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    organization_id uuid NOT NULL,
    person_id uuid,
    raw_record_id uuid NOT NULL,
    conversation_started_at timestamp with time zone NOT NULL,
    status text NOT NULL,
    skip_reason text,
    repo_name text,
    source_content_sha256 text,
    source_updated_at timestamp with time zone,
    spine_version integer,
    vocabulary_version integer NOT NULL,
    call_state jsonb,
    contract_version_read integer,
    contract_version_evidence integer,
    contract_version_discard integer,
    attempt_count integer DEFAULT 0 NOT NULL,
    last_error text,
    last_attempted_at timestamp with time zone,
    last_succeeded_at timestamp with time zone,
    children_seq integer DEFAULT 0 NOT NULL,
    folded_children_seq integer DEFAULT 0 NOT NULL,
    flag_skipped_producers smallint DEFAULT 0 NOT NULL,
    claim_token uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    pending_data_producers smallint DEFAULT 0 NOT NULL,
    failed_producers smallint DEFAULT 0 NOT NULL,
    origin text
);
CREATE TABLE public.session_phase_analyses (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    organization_id uuid NOT NULL,
    session_analysis_id uuid,
    conversation_started_at timestamp with time zone NOT NULL,
    phase_ordinal integer,
    range_start integer,
    range_end integer,
    phase_theme text,
    sdlc_stage_set jsonb,
    task_complexity text,
    user_steering text,
    phase_objective text,
    phase_objective_met text,
    agent_helpfulness text,
    smoothness text,
    friction_cause text,
    speed text,
    boundary_overridden boolean,
    override_reason text,
    hitl_gate text,
    turn_count integer,
    tokens bigint,
    tool_call_count integer,
    went_well jsonb,
    went_wrong jsonb,
    coaching jsonb,
    segmentation_suspect boolean,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);
CREATE TABLE public.session_round_trip_faults (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    organization_id uuid NOT NULL,
    session_analysis_id uuid NOT NULL,
    raw_record_id uuid NOT NULL,
    conversation_started_at timestamp with time zone NOT NULL,
    prompt_index integer NOT NULL,
    fault text,
    evidence_prompt_index integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);
CREATE TABLE public.session_turn_analyses (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    organization_id uuid NOT NULL,
    raw_record_id uuid NOT NULL,
    session_analysis_id uuid,
    conversation_started_at timestamp with time zone NOT NULL,
    prompt_index integer NOT NULL,
    prompt_origination text,
    author text NOT NULL,
    is_subagent boolean DEFAULT false NOT NULL,
    prompt_type text,
    user_steering text,
    prompt_completeness text,
    prompt_objective_met text,
    agent_helpfulness text,
    agent_initiative text,
    tool_use text,
    was_retry text,
    failure_class text,
    token_locus text,
    waste_cause text,
    decision_type text,
    downstream_impact text,
    feedback_target text,
    feedback_disposition text,
    referenced_artifact text,
    referenced_finding_ids jsonb,
    round_trip_fault text,
    round_trip_evidence_prompt_index integer,
    prompt_issues jsonb,
    judged_content_sha256 text,
    models jsonb,
    token_breakdown jsonb,
    token_fidelity text NOT NULL,
    input_tokens bigint GENERATED ALWAYS AS (
CASE
    WHEN (token_breakdown IS NULL) THEN NULL::bigint
    ELSE COALESCE(((token_breakdown ->> 'inputTokens'::text))::bigint, (0)::bigint)
END) STORED,
    output_tokens bigint GENERATED ALWAYS AS (
CASE
    WHEN (token_breakdown IS NULL) THEN NULL::bigint
    ELSE COALESCE(((token_breakdown ->> 'outputTokens'::text))::bigint, (0)::bigint)
END) STORED,
    cache_read_tokens bigint GENERATED ALWAYS AS (
CASE
    WHEN (token_breakdown IS NULL) THEN NULL::bigint
    ELSE COALESCE(((token_breakdown ->> 'cacheReadTokens'::text))::bigint, (0)::bigint)
END) STORED,
    cache_write_tokens bigint GENERATED ALWAYS AS (
CASE
    WHEN (token_breakdown IS NULL) THEN NULL::bigint
    ELSE COALESCE(((token_breakdown ->> 'cacheWriteTokens'::text))::bigint, (0)::bigint)
END) STORED,
    native_billing_units numeric,
    lines_added integer,
    lines_removed integer,
    tool_call_count integer,
    tool_error_count integer,
    tools jsonb,
    tool_retry_count integer,
    api_retry_count integer,
    compaction_count integer,
    files_touched jsonb,
    surface text,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    repo_full_name text,
    context_tokens bigint,
    context_pct numeric,
    subagent_count integer,
    parent_conversation_id text,
    parent_prompt_index integer,
    parent_tool_use_id text,
    source_content_sha256 text NOT NULL,
    phase_id uuid,
    evidence_seq integer,
    chunk_index integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);
ALTER TABLE ONLY public.harness_artifact_classifications
    ADD CONSTRAINT harness_artifact_classifications_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.harness_artifact_classifications
    ADD CONSTRAINT harness_artifact_classifications_uniq UNIQUE (organization_id, task, harness_artifact_id);
ALTER TABLE ONLY public.harness_artifacts
    ADD CONSTRAINT harness_artifacts_identity_uniq UNIQUE NULLS NOT DISTINCT (organization_id, person_id, ide_type, repository_id, source_path);
ALTER TABLE ONLY public.harness_artifacts
    ADD CONSTRAINT harness_artifacts_org_id_uniq UNIQUE (organization_id, id);
ALTER TABLE ONLY public.harness_artifacts
    ADD CONSTRAINT harness_artifacts_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.raw_records
    ADD CONSTRAINT raw_records_dedup UNIQUE (organization_id, connector_id, operation_id, record_key, data_date);
ALTER TABLE ONLY public.raw_records
    ADD CONSTRAINT raw_records_pkey PRIMARY KEY (id, data_date);
ALTER TABLE ONLY public.security_flags
    ADD CONSTRAINT security_flags_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.session_analyses
    ADD CONSTRAINT session_analyses_generation_key UNIQUE (organization_id, raw_record_id, vocabulary_version);
ALTER TABLE ONLY public.session_analyses
    ADD CONSTRAINT session_analyses_org_id_key UNIQUE (organization_id, id);
ALTER TABLE ONLY public.session_analyses
    ADD CONSTRAINT session_analyses_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.session_analysis_state
    ADD CONSTRAINT session_analysis_state_generation_key UNIQUE (organization_id, raw_record_id, vocabulary_version);
ALTER TABLE ONLY public.session_analysis_state
    ADD CONSTRAINT session_analysis_state_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.session_phase_analyses
    ADD CONSTRAINT session_phase_analyses_ordinal_key UNIQUE (organization_id, session_analysis_id, phase_ordinal);
ALTER TABLE ONLY public.session_phase_analyses
    ADD CONSTRAINT session_phase_analyses_org_id_key UNIQUE (organization_id, id);
ALTER TABLE ONLY public.session_phase_analyses
    ADD CONSTRAINT session_phase_analyses_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.session_round_trip_faults
    ADD CONSTRAINT session_round_trip_faults_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.session_turn_analyses
    ADD CONSTRAINT session_turn_analyses_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.session_turn_analyses
    ADD CONSTRAINT session_turn_analyses_writer_key UNIQUE (organization_id, raw_record_id, prompt_index);
CREATE INDEX harness_artifact_classifications_by_org_task ON public.harness_artifact_classifications USING btree (organization_id, task) WHERE (override_set_by_person_id IS NULL);
CREATE INDEX harness_artifact_classifications_by_verdicts ON public.harness_artifact_classifications USING gin (verdicts_by_version jsonb_path_ops);
CREATE INDEX harness_artifact_classifications_lease_expiry ON public.harness_artifact_classifications USING btree (organization_id, task, lease_expires_at) WHERE (attempt_state = 'HARNESS_CLASSIFICATION_ATTEMPT_STATE_IN_FLIGHT'::text);
CREATE UNIQUE INDEX harness_artifact_classifications_one_in_flight ON public.harness_artifact_classifications USING btree (organization_id, task, attempting_version, attempting_digest) WHERE (attempt_state = 'HARNESS_CLASSIFICATION_ATTEMPT_STATE_IN_FLIGHT'::text);
CREATE INDEX idx_raw_records_actionable ON ONLY public.raw_records USING btree (connector_id, organization_id, data_date, processing_status);
CREATE INDEX idx_raw_records_actionable_dates ON ONLY public.raw_records USING btree (connector_id, organization_id, data_date, processing_status, retry_count) WHERE ((deleted_at IS NULL) AND ((processing_status)::text = ANY ((ARRAY['pending'::character varying, 'skipped_no_seat'::character varying, 'skipped_out_of_window'::character varying, 'failed'::character varying])::text[])));
CREATE INDEX idx_raw_records_actionable_org ON ONLY public.raw_records USING btree (connector_id, organization_id, processing_status, data_date, retry_count) WHERE (deleted_at IS NULL);
CREATE INDEX idx_raw_records_day_drain ON ONLY public.raw_records USING btree (organization_id, connector_id, data_date, operation_id, record_key) WHERE (deleted_at IS NULL);
CREATE INDEX idx_raw_records_fetch_commits ON ONLY public.raw_records USING btree (organization_id, connector_id, ((metadata ->> 'repo'::text)), data_date) WHERE (((operation_id)::text = 'fetch_commits'::text) AND (deleted_at IS NULL));
CREATE INDEX idx_raw_records_fetched ON ONLY public.raw_records USING btree (organization_id, connector_id, fetched_at);
CREATE INDEX idx_raw_records_parent_lineage ON ONLY public.raw_records USING btree (organization_id, ((metadata ->> 'parent_record_key'::text)), data_date) WHERE (((metadata ->> 'parent_record_key'::text) IS NOT NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_raw_records_pr_author ON ONLY public.raw_records USING btree (organization_id, connector_id, ((metadata ->> 'pr_author_id'::text)), data_date) WHERE (((operation_id)::text = 'fetch_prs'::text) AND (deleted_at IS NULL));
CREATE INDEX idx_raw_records_pr_circles_lookup ON ONLY public.raw_records USING btree (organization_id, connector_id, operation_id, fetched_at) WHERE (((operation_id)::text = ANY ((ARRAY['pr_sync_events'::character varying, 'fetch_reviews'::character varying])::text[])) AND (deleted_at IS NULL));
CREATE INDEX idx_raw_records_processing ON ONLY public.raw_records USING btree (organization_id, connector_id, operation_id, processing_status) WHERE ((processing_status)::text = 'pending'::text);
CREATE INDEX idx_raw_records_session_recency ON ONLY public.raw_records USING btree (organization_id, updated_at) WHERE (((connector_id)::text = 'session'::text) AND (deleted_at IS NULL));
CREATE INDEX idx_raw_records_relation_type ON ONLY public.raw_records USING btree (organization_id, ((metadata ->> 'relation_type'::text)), data_date) WHERE (((metadata ->> 'relation_type'::text) IS NOT NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_security_flags_org_date_id ON public.security_flags USING btree (organization_id, flag_date, id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_org_flag_type_id ON public.security_flags USING btree (organization_id, flag_type, id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_org_severity_id ON public.security_flags USING btree (organization_id, severity, id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_org_severity_rank_id ON public.security_flags USING btree (organization_id, (
CASE severity
    WHEN 'critical'::text THEN 4
    WHEN 'high'::text THEN 3
    WHEN 'medium'::text THEN 2
    WHEN 'low'::text THEN 1
    ELSE 0
END), id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_org_status_id ON public.security_flags USING btree (organization_id, status, id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_person ON public.security_flags USING btree (person_id, flag_date) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_person_date_id ON public.security_flags USING btree (organization_id, person_id, flag_date, id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_person_flag_type_id ON public.security_flags USING btree (organization_id, person_id, flag_type, id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_person_severity_id ON public.security_flags USING btree (organization_id, person_id, severity, id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_person_severity_rank_id ON public.security_flags USING btree (organization_id, person_id, (
CASE severity
    WHEN 'critical'::text THEN 4
    WHEN 'high'::text THEN 3
    WHEN 'medium'::text THEN 2
    WHEN 'low'::text THEN 1
    ELSE 0
END), id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_person_status_id ON public.security_flags USING btree (organization_id, person_id, status, id) WHERE (deleted_at IS NULL);
CREATE INDEX idx_security_flags_raw_record_id_status ON public.security_flags USING btree (raw_record_id, status) WHERE (deleted_at IS NULL);
CREATE UNIQUE INDEX idx_security_flags_raw_record_type_unique ON public.security_flags USING btree (raw_record_id, flag_type) WHERE (deleted_at IS NULL);
CREATE INDEX idx_session_analyses_ide_date ON public.session_analyses USING btree (ide_type, conversation_started_at, organization_id) WHERE (deleted_at IS NULL);
CREATE INDEX ix_artifact_component ON public.harness_artifacts USING btree (organization_id, ide_type, type, name);
CREATE INDEX ix_artifact_deviation ON public.harness_artifacts USING btree (organization_id, deviation_state);
CREATE INDEX ix_artifact_person ON public.harness_artifacts USING btree (organization_id, person_id);
CREATE INDEX ix_artifact_person_path ON public.harness_artifacts USING btree (organization_id, person_id, source_path) WHERE (deleted_at IS NULL);
CREATE INDEX ix_artifact_repo ON public.harness_artifacts USING btree (organization_id, repository_id);
CREATE INDEX ix_artifact_stale ON public.harness_artifacts USING btree (last_synced_at) WHERE (deleted_at IS NULL);
CREATE INDEX ix_artifact_vcs_master ON public.harness_artifacts USING btree (organization_id, repository_id, source_path) WHERE ((deleted_at IS NULL) AND (artifact_source = 'HARNESS_ARTIFACT_SOURCE_VCS'::text) AND (scope = 'HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED'::text));
CREATE INDEX ix_artifact_vcs_tombstone ON public.harness_artifacts USING btree (deleted_at) WHERE ((deleted_at IS NOT NULL) AND (artifact_source = 'HARNESS_ARTIFACT_SOURCE_VCS'::text));
CREATE INDEX ix_harness_artifacts_files_fileids ON public.harness_artifacts USING gin (((files -> 'files'::text)));
CREATE INDEX session_analyses_keyset ON public.session_analyses USING btree (organization_id, person_id, conversation_started_at, id) WHERE (deleted_at IS NULL);
CREATE INDEX session_analyses_keyset_iterations ON public.session_analyses USING btree (organization_id, person_id, iteration_count, id) WHERE (deleted_at IS NULL);
CREATE INDEX session_analyses_keyset_score ON public.session_analyses USING btree (organization_id, person_id, score_overall, id) WHERE (deleted_at IS NULL);
CREATE INDEX session_analyses_keyset_wasted ON public.session_analyses USING btree (organization_id, person_id, wasted_tokens, id) WHERE (deleted_at IS NULL);
CREATE INDEX session_analyses_keyset_work_type ON public.session_analyses USING btree (organization_id, person_id, work_type, id) WHERE (deleted_at IS NULL);
CREATE UNIQUE INDEX session_analyses_one_live ON public.session_analyses USING btree (organization_id, raw_record_id) WHERE ((superseded_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX session_analyses_org_started_at ON public.session_analyses USING btree (organization_id, conversation_started_at) WHERE (deleted_at IS NULL);
CREATE INDEX session_analyses_org_started_entrypoint ON public.session_analyses USING btree (organization_id, ((session_meta_raw ->> 'entrypoint'::text)), conversation_started_at) WHERE (deleted_at IS NULL);
CREATE INDEX session_analyses_org_started_ide ON public.session_analyses USING btree (organization_id, conversation_started_at, ide_type) WHERE (deleted_at IS NULL);
CREATE INDEX session_analyses_org_started_project ON public.session_analyses USING btree (organization_id, conversation_started_at, project_name) WHERE (deleted_at IS NULL);
CREATE INDEX session_analyses_org_started_session_type ON public.session_analyses USING btree (organization_id, session_type, conversation_started_at) WHERE (deleted_at IS NULL);
CREATE INDEX session_analyses_org_started_work_type ON public.session_analyses USING btree (organization_id, conversation_started_at, work_type) WHERE (deleted_at IS NULL);
CREATE INDEX session_analysis_state_failed_producers ON public.session_analysis_state USING btree (organization_id) WHERE ((status = ANY (ARRAY['SESSION_ANALYSIS_STATUS_JUDGED'::text, 'SESSION_ANALYSIS_STATUS_SPINE_DONE'::text])) AND (failed_producers <> 0));
CREATE UNIQUE INDEX session_analysis_state_one_live ON public.session_analysis_state USING btree (organization_id, raw_record_id) WHERE ((status <> 'SESSION_ANALYSIS_STATUS_SUPERSEDED'::text) AND (deleted_at IS NULL));
CREATE INDEX session_analysis_state_org_status ON public.session_analysis_state USING btree (organization_id, status) WHERE (deleted_at IS NULL);
CREATE INDEX session_analysis_state_pending_data ON public.session_analysis_state USING btree (organization_id) WHERE ((status = ANY (ARRAY['SESSION_ANALYSIS_STATUS_JUDGED'::text, 'SESSION_ANALYSIS_STATUS_SPINE_DONE'::text])) AND (pending_data_producers <> 0));
CREATE INDEX session_analysis_state_producer_enabled ON public.session_analysis_state USING btree (organization_id) WHERE ((status = ANY (ARRAY['SESSION_ANALYSIS_STATUS_JUDGED'::text, 'SESSION_ANALYSIS_STATUS_SPINE_DONE'::text])) AND (flag_skipped_producers <> 0));
CREATE INDEX session_analysis_state_readmit_no_seat ON public.session_analysis_state USING btree (organization_id, person_id) WHERE ((status = 'SESSION_ANALYSIS_STATUS_SKIPPED'::text) AND (skip_reason = 'SESSION_ANALYSIS_SKIP_REASON_NO_SEAT'::text));
CREATE INDEX session_analysis_state_readmit_out_of_window ON public.session_analysis_state USING btree (organization_id, conversation_started_at) WHERE ((status = 'SESSION_ANALYSIS_STATUS_SKIPPED'::text) AND (skip_reason = 'SESSION_ANALYSIS_SKIP_REASON_OUT_OF_WINDOW'::text));
CREATE INDEX session_analysis_state_readmit_repo_untracked ON public.session_analysis_state USING btree (organization_id, repo_name) WHERE ((status = 'SESSION_ANALYSIS_STATUS_SKIPPED'::text) AND (skip_reason = ANY (ARRAY['SESSION_ANALYSIS_SKIP_REASON_REPO_UNTRACKED_METADATA'::text, 'SESSION_ANALYSIS_SKIP_REASON_REPO_UNTRACKED_PROJECT'::text])));
CREATE INDEX session_phase_analyses_parent ON public.session_phase_analyses USING btree (organization_id, session_analysis_id) WHERE (deleted_at IS NULL);
CREATE INDEX session_round_trip_faults_parent ON public.session_round_trip_faults USING btree (organization_id, session_analysis_id) WHERE (deleted_at IS NULL);
CREATE UNIQUE INDEX session_round_trip_faults_writer_key ON public.session_round_trip_faults USING btree (organization_id, session_analysis_id, prompt_index) WHERE (deleted_at IS NULL);
CREATE INDEX session_turn_analyses_org_started ON public.session_turn_analyses USING btree (organization_id, conversation_started_at) WHERE (deleted_at IS NULL);
CREATE INDEX session_turn_analyses_parent_locator ON public.session_turn_analyses USING btree (organization_id, parent_conversation_id, parent_prompt_index) WHERE ((parent_conversation_id IS NOT NULL) AND (deleted_at IS NULL));

-- raw_records is range-partitioned by data_date; a DEFAULT partition accepts any date in tests.
CREATE TABLE IF NOT EXISTS public.raw_records_default PARTITION OF public.raw_records DEFAULT;
