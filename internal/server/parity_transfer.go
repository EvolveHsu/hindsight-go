package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-faster/jx"

	ht "github.com/ogen-go/ogen/http"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
	"github.com/EvolveHsu/hindsight-go/internal/transfer"
)

// newTransferID returns a random hex id for operation and storage-key names.
func newTransferID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// TransferSource is the storage slice behind the document/bank transfer
// endpoints: read a bank's documents and observations out, write them back.
// Both backends implement it; the in-memory backend exports neither
// observations nor attachment bytes, so its observation list is empty.
type TransferSource interface {
	ExportTransferDocuments(ctx context.Context, bankID string, docIDs []string) ([]model.TransferDocument, error)
	ExportTransferObservations(ctx context.Context, bankID string) ([]model.TransferObservation, error)
	RestoreTransferDocument(ctx context.Context, bankID string, doc model.TransferDocument, onConflict string) (model.TransferOutcome, error)
	RestoreTransferObservation(ctx context.Context, bankID string, obs model.TransferObservation) error
}

func (e *Engine) transferSource() (TransferSource, error) {
	if s, ok := e.store.(TransferSource); ok {
		return s, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support document transfer"}}
}

// transferOptions selects which archive sections an export carries.
type transferOptions struct {
	ArchiveType   string
	Observations  bool
	BankConfig    bool
	KnowledgeBase bool
}

// buildTransferArchive assembles a ZIP archive for a bank or a document subset.
func (e *Engine) buildTransferArchive(ctx context.Context, bankID string, docIDs []string, opts transferOptions) ([]byte, error) {
	src, err := e.transferSource()
	if err != nil {
		return nil, err
	}
	docs, err := src.ExportTransferDocuments(ctx, bankID, docIDs)
	if err != nil {
		return nil, internal(err)
	}
	sections := transfer.Sections{}
	if opts.Observations {
		obs, err := src.ExportTransferObservations(ctx, bankID)
		if err != nil {
			return nil, internal(err)
		}
		sections.Observations = obs
	}
	if opts.BankConfig {
		if cfgSrc, ok := e.store.(BankConfigSource); ok {
			cfg, err := cfgSrc.BankConfig(ctx, bankID)
			if err != nil {
				return nil, internal(err)
			}
			sections.Bank = &transfer.BankRow{BankID: bankID, Config: cfg}
		}
		if dirSrc, ok := e.store.(DirectiveSource); ok {
			list, err := dirSrc.ListDirectives(ctx, bankID)
			if err != nil {
				return nil, internal(err)
			}
			for _, d := range list {
				sections.Directives = append(sections.Directives, transfer.DirectiveRow{ID: d.ID, Name: d.Name, Content: d.Content})
			}
		}
	}
	if opts.KnowledgeBase {
		if mms, ok := e.store.(MentalModelSource); ok {
			list, err := mms.ListMentalModels(ctx, bankID)
			if err != nil {
				return nil, internal(err)
			}
			for _, m := range list {
				sections.MentalModels = append(sections.MentalModels, transfer.MentalModelRow{
					ID: m.ID, Name: m.Name, SourceQuery: m.SourceQuery, Tags: m.Tags, MaxTokens: m.MaxTokens,
				})
			}
		}
		if ks, ok := e.store.(KnowledgeSource); ok {
			rows, err := ks.ListKnowledgeRows(ctx, bankID)
			if err != nil {
				return nil, internal(err)
			}
			for _, r := range rows {
				sections.KnowledgePages = append(sections.KnowledgePages, transfer.KnowledgeRow{
					ID: r.ID, ParentID: r.ParentID, Kind: r.Kind, Name: r.Name,
					MentalModelID: r.MentalModelID, SortOrder: r.SortOrder, Managed: r.Managed,
					Description: r.Description, Tags: r.Tags,
				})
			}
		}
	}

	scope := &transfer.Scope{
		Data:       opts.Observations || opts.KnowledgeBase,
		BankConfig: opts.BankConfig,
	}
	manifest := transfer.Manifest{SourceBankID: bankID, ArchiveType: opts.ArchiveType, Scope: scope}
	var buf bytes.Buffer
	if err := transfer.Write(&buf, manifest, docs, sections); err != nil {
		return nil, internal(err)
	}
	return buf.Bytes(), nil
}

// storeArchive writes an archive to the blob store and returns its key.
func (e *Engine) storeArchive(bankID, name string, data []byte) (string, error) {
	// The generated route for download_file makes {key} a single path segment,
	// so the key the archive is stored under must not contain a slash. The
	// bank id travels inside the key and is resolved back through the
	// operation registry on download.
	key := fmt.Sprintf("%s-exports-%s-%s", sanitizeKeyPart(bankID), newTransferID(), name)
	if err := e.fileStore().Put(key, data); err != nil {
		return "", internal(err)
	}
	return key, nil
}

// sanitizeKeyPart keeps a bank id usable inside a single-segment storage key.
func sanitizeKeyPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "bank"
	}
	return b.String()
}

// ExportDocuments implements export_documents
// (POST /banks/{bank_id}/document-transfer/export).
//
// Upstream queues the export and returns an operation id; the lite build runs
// it inline, then records the finished operation so polling its id returns the
// same download_url a queued export would have produced.
func (e *Engine) ExportDocuments(ctx context.Context, params api.ExportDocumentsParams) (api.ExportDocumentsRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("Bank '%s' not found", params.BankID)
	}
	docIDs := params.DocumentID
	includeObs := params.IncludeObservations.Or(false)
	includeKB := params.IncludeKnowledgeBase.Or(false)
	if (includeObs || includeKB) && len(docIDs) > 0 {
		return nil, badRequest("include_observations and include_knowledge_base are only supported when exporting the whole bank (omit document_id)")
	}
	data, err := e.buildTransferArchive(ctx, params.BankID, docIDs, transferOptions{
		ArchiveType:   transfer.ArchiveDocuments,
		Observations:  includeObs,
		KnowledgeBase: includeKB,
	})
	if err != nil {
		return nil, err
	}
	key, err := e.storeArchive(params.BankID, "documents.zip", data)
	if err != nil {
		return nil, err
	}
	opID := recordOperation(params.BankID, "export_documents", downloadMetadata(params.BankID, key, len(data), params.BankID+"-documents.zip"))
	return &api.DocumentExportSubmitResponse{OperationID: opID, Status: api.OptString{Set: true, Value: "completed"}}, nil
}

// ExportBankTransfer implements export_bank_transfer
// (POST /banks/{bank_id}/transfer/export).
func (e *Engine) ExportBankTransfer(ctx context.Context, params api.ExportBankTransferParams) (api.ExportBankTransferRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("Bank '%s' not found", params.BankID)
	}
	includeData := params.IncludeData.Or(true)
	includeBankConfig := params.IncludeBankConfig.Or(true)
	includeHistory := params.IncludeHistory.Or(false)
	if !includeData && !includeBankConfig && !includeHistory {
		return nil, badRequest("Nothing to export: set at least one of include_data, include_bank_config, include_history")
	}
	docIDs := params.DocumentID
	if len(docIDs) > 0 && (includeBankConfig || includeHistory) {
		return nil, badRequest("include_bank_config and include_history are only supported for a whole-bank export (omit document_id)")
	}
	data, err := e.buildTransferArchive(ctx, params.BankID, docIDs, transferOptions{
		ArchiveType:   transfer.ArchiveBank,
		Observations:  includeData,
		BankConfig:    includeBankConfig && len(docIDs) == 0,
		KnowledgeBase: includeData && len(docIDs) == 0,
	})
	if err != nil {
		return nil, err
	}
	key, err := e.storeArchive(params.BankID, "bank.zip", data)
	if err != nil {
		return nil, err
	}
	opID := recordOperation(params.BankID, "export_bank_transfer", downloadMetadata(params.BankID, key, len(data), params.BankID+"-bank.zip"))
	return &api.BankTransferSubmitResponse{OperationID: opID, Status: api.OptString{Set: true, Value: "completed"}}, nil
}

func downloadMetadata(bankID, key string, size int, filename string) map[string]any {
	return map[string]any{
		"download_url": "/v1/default/files/download/" + key,
		"storage_key":  key,
		"byte_size":    size,
		"filename":     filename,
	}
}

// ExportDocumentsSyncRemoved implements export_documents_sync_removed
// (GET /banks/{bank_id}/document-transfer). Upstream removed the synchronous
// export; the endpoint answers 410 pointing at the async pair.
func (e *Engine) ExportDocumentsSyncRemoved(ctx context.Context, params api.ExportDocumentsSyncRemovedParams) (api.ExportDocumentsSyncRemovedRes, error) {
	return nil, &statusError{
		status: http.StatusGone,
		body: map[string]any{"detail": fmt.Sprintf(
			"Synchronous document export has been removed because it could take down the shared API on "+
				"large banks. Submit an async export via POST /v1/default/banks/%s/document-transfer/export, "+
				"poll GET /v1/default/banks/%s/operations/{operation_id}, then download the archive from "+
				"the download_url in the operation's result_metadata.", params.BankID, params.BankID)},
	}
}

// ImportDocuments implements import_documents (POST /banks/{bank_id}/document-transfer).
func (e *Engine) ImportDocuments(ctx context.Context, req *api.BodyImportDocumentsMultipart, params api.ImportDocumentsParams) (api.ImportDocumentsRes, error) {
	onConflict := params.OnConflict.Or("skip")
	if err := validConflictMode(onConflict); err != nil {
		return nil, err
	}
	data, err := readMultipartFile(req.File)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	restored, skipped, err := e.restoreArchiveDocuments(ctx, params.BankID, data, onConflict)
	if err != nil {
		return nil, err
	}
	opID := recordOperation(params.BankID, "import_documents", map[string]any{
		"documents_imported": restored, "documents_skipped": skipped,
	})
	return &api.DocumentImportSubmitResponse{OperationID: opID, Status: api.OptString{Set: true, Value: "completed"}}, nil
}

// ImportBankTransfer implements import_bank_transfer
// (POST /banks/{bank_id}/transfer/import).
func (e *Engine) ImportBankTransfer(ctx context.Context, req *api.BodyImportBankTransferMultipart, params api.ImportBankTransferParams) (api.ImportBankTransferRes, error) {
	mode := params.Mode.Or("restore")
	if mode != "restore" && mode != "merge" {
		return nil, badRequest("Invalid mode '%s' (expected restore|merge)", mode)
	}
	conflict := params.DocumentConflict.Or("skip")
	if err := validConflictMode(conflict); err != nil {
		return nil, err
	}
	target := params.TargetBankID.Or("")
	if mode == "merge" && target != "" {
		return nil, badRequest("target_bank_id is only valid in restore mode; merge imports into %s", params.BankID)
	}
	if mode == "merge" && (params.IncludeData.Set || params.IncludeBankConfig.Set || params.IncludeHistory.Set) {
		return nil, badRequest("include_data / include_bank_config / include_history apply to mode=restore; a merge imports the archive's documents only")
	}
	data, err := readMultipartFile(req.File)
	if err != nil {
		return nil, badRequest("%v", err)
	}

	manifest, _, err := transfer.Read(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, badRequest("%v", err)
	}
	sections, err := transfer.ReadSections(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, badRequest("%v", err)
	}

	if mode == "merge" {
		restored, skipped, err := e.restoreArchiveDocuments(ctx, params.BankID, data, conflict)
		if err != nil {
			return nil, err
		}
		opID := recordOperation(params.BankID, "import_bank_transfer", map[string]any{
			"mode": "merge", "documents_imported": restored, "documents_skipped": skipped,
		})
		return &api.BankTransferSubmitResponse{OperationID: opID, Status: api.OptString{Set: true, Value: "completed"}}, nil
	}

	// Restore writes into a fresh bank; an existing target is a caller error.
	if target == "" {
		target = manifest.SourceBankID
	}
	if strings.TrimSpace(target) == "" {
		return nil, badRequest("archive names no source bank and no target_bank_id was given")
	}
	if existing, err := e.store.GetBank(ctx, target); err != nil {
		return nil, internal(err)
	} else if existing != nil {
		return nil, badRequest("Target bank '%s' already exists; restore requires a fresh bank id", target)
	}
	if _, err := e.store.EnsureBank(ctx, target, "", ""); err != nil {
		return nil, internal(err)
	}

	includeData := params.IncludeData.Or(true)
	includeConfig := params.IncludeBankConfig.Or(true)

	if err := e.restoreArchiveSections(ctx, target, sections, includeConfig, true); err != nil {
		return nil, err
	}
	restored, skipped := 0, 0
	if includeData {
		if restored, skipped, err = e.restoreArchiveDocumentsWithSections(ctx, target, data, manifest, sections); err != nil {
			return nil, err
		}
	}
	opID := recordOperation(params.BankID, "import_bank_transfer", map[string]any{
		"mode": "restore", "target_bank_id": target,
		"documents_imported": restored, "documents_skipped": skipped,
	})
	return &api.BankTransferSubmitResponse{OperationID: opID, Status: api.OptString{Set: true, Value: "completed"}}, nil
}

func validConflictMode(mode string) error {
	switch mode {
	case "skip", "replace", "new-id":
		return nil
	default:
		return badRequest("Invalid on_conflict '%s' (expected skip|replace|new-id)", mode)
	}
}

func readMultipartFile(f ht.MultipartFile) ([]byte, error) {
	if f.File == nil {
		return nil, fmt.Errorf("transfer archive is required")
	}
	data, err := io.ReadAll(io.LimitReader(f.File, 512<<20))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("transfer archive is empty")
	}
	return data, nil
}

// restoreArchiveDocuments restores every document in an archive.
func (e *Engine) restoreArchiveDocuments(ctx context.Context, bankID string, data []byte, onConflict string) (int, int, error) {
	_, docs, err := transfer.Read(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, 0, badRequest("%v", err)
	}
	return e.restoreDocuments(ctx, bankID, docs, onConflict)
}

// restoreArchiveDocumentsWithSections restores documents, then the
// observations the archive carried.
func (e *Engine) restoreArchiveDocumentsWithSections(ctx context.Context, bankID string, data []byte, manifest *transfer.Manifest, sections *transfer.Sections) (int, int, error) {
	_, docs, err := transfer.Read(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, 0, badRequest("%v", err)
	}
	restored, skipped, err := e.restoreDocuments(ctx, bankID, docs, "skip")
	if err != nil {
		return 0, 0, err
	}
	if len(sections.Observations) > 0 {
		src, err := e.transferSource()
		if err != nil {
			return 0, 0, err
		}
		for _, obs := range sections.Observations {
			if err := src.RestoreTransferObservation(ctx, bankID, obs); err != nil {
				return 0, 0, internal(err)
			}
		}
	}
	return restored, skipped, nil
}

func (e *Engine) restoreDocuments(ctx context.Context, bankID string, docs []model.TransferDocument, onConflict string) (int, int, error) {
	src, err := e.transferSource()
	if err != nil {
		return 0, 0, err
	}
	if b, err := e.store.GetBank(ctx, bankID); err != nil {
		return 0, 0, internal(err)
	} else if b == nil {
		return 0, 0, notFound("Bank '%s' not found", bankID)
	}
	restored, skipped := 0, 0
	for _, doc := range docs {
		out, err := src.RestoreTransferDocument(ctx, bankID, doc, onConflict)
		if err != nil {
			return 0, 0, internal(err)
		}
		if out.Skipped {
			skipped++
			continue
		}
		restored++
	}
	return restored, skipped, nil
}

// restoreArchiveSections restores the whole-bank sections that are not
// documents: bank config, directives, mental models and knowledge pages.
func (e *Engine) restoreArchiveSections(ctx context.Context, bankID string, sections *transfer.Sections, includeConfig, includeKnowledge bool) error {
	if includeConfig {
		if sections.Bank != nil && len(sections.Bank.Config) > 0 {
			if cfgSrc, ok := e.store.(BankConfigSource); ok {
				if err := cfgSrc.UpdateBankConfig(ctx, bankID, sections.Bank.Config); err != nil {
					return internal(err)
				}
			}
		}
		if len(sections.Directives) > 0 {
			ds, ok := e.store.(DirectiveSource)
			if !ok {
				return &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support directives"}}
			}
			for _, d := range sections.Directives {
				if _, err := ds.CreateDirective(ctx, bankID, d.Name, d.Content); err != nil {
					return internal(err)
				}
			}
		}
	}
	if includeKnowledge {
		if len(sections.MentalModels) > 0 {
			mms, err := e.mmStore()
			if err != nil {
				return err
			}
			existing, err := mms.ListMentalModels(ctx, bankID)
			if err != nil {
				return internal(err)
			}
			byID := map[string]*model.MentalModel{}
			for _, m := range existing {
				byID[m.ID] = m
			}
			for _, row := range sections.MentalModels {
				if _, ok := byID[row.ID]; ok {
					if _, err := mms.UpdateMentalModel(ctx, bankID, row.ID, func(m *model.MentalModel) {
						m.Name = row.Name
						m.SourceQuery = row.SourceQuery
						m.Tags = row.Tags
						if row.MaxTokens > 0 {
							m.MaxTokens = row.MaxTokens
						}
					}); err != nil {
						return internal(err)
					}
					continue
				}
				m := &model.MentalModel{ID: row.ID, Name: row.Name, SourceQuery: row.SourceQuery, Tags: row.Tags, MaxTokens: row.MaxTokens}
				created, err := mms.CreateMentalModel(ctx, bankID, m)
				if err != nil {
					return internal(err)
				}
				if e.provider != nil {
					e.refreshMentalModelNow(ctx, mms, bankID, created)
				}
			}
		}
		if len(sections.KnowledgePages) > 0 {
			ks, ok := e.store.(KnowledgeSource)
			if !ok {
				return &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support the knowledge base"}}
			}
			// Parents first so a page's parent_id already resolves.
			ordered := knowledgeParentsFirst(sections.KnowledgePages)
			for _, r := range ordered {
				err := ks.CreateKnowledgeRow(ctx, bankID, model.KnowledgeRow{
					ID: r.ID, ParentID: r.ParentID, Kind: r.Kind, Name: r.Name,
					MentalModelID: r.MentalModelID, SortOrder: r.SortOrder, Managed: r.Managed,
					Description: r.Description, Tags: r.Tags,
				})
				if err != nil {
					return internal(err)
				}
			}
		}
	}
	return nil
}

// knowledgeParentsFirst orders rows so every parent precedes its children.
func knowledgeParentsFirst(rows []transfer.KnowledgeRow) []transfer.KnowledgeRow {
	byID := map[string]transfer.KnowledgeRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	out := make([]transfer.KnowledgeRow, 0, len(rows))
	seen := map[string]bool{}
	var visit func(r transfer.KnowledgeRow)
	visit = func(r transfer.KnowledgeRow) {
		if seen[r.ID] {
			return
		}
		if parent, ok := byID[r.ParentID]; ok && r.ParentID != "" && !seen[parent.ID] {
			visit(parent)
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	for _, r := range rows {
		visit(r)
	}
	return out
}

// GetBankTemplateSchema implements get_bank_template_schema
// (GET /bank-template-schema): the JSON Schema the docs site serves verbatim.
func (e *Engine) GetBankTemplateSchema(ctx context.Context) (jx.Raw, error) {
	return jx.Raw(bankTemplateSchema), nil
}

// ExportBankTemplate implements export_bank_template (GET /banks/{bank_id}/export).
func (e *Engine) ExportBankTemplate(ctx context.Context, params api.ExportBankTemplateParams) (api.ExportBankTemplateRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("Bank '%s' not found", params.BankID)
	}

	out := &api.BankTemplateManifest{Version: bankTemplateVersion}
	if cfgSrc, ok := e.store.(BankConfigSource); ok {
		cfg, err := cfgSrc.BankConfig(ctx, params.BankID)
		if err != nil {
			return nil, internal(err)
		}
		if len(cfg) > 0 {
			tmpl := bankTemplateConfigFromMap(cfg)
			out.Bank = api.OptBankTemplateConfig{Set: true, Value: tmpl}
		}
	}
	if mms, ok := e.store.(MentalModelSource); ok {
		list, err := mms.ListMentalModels(ctx, params.BankID)
		if err != nil {
			return nil, internal(err)
		}
		for _, m := range list {
			out.MentalModels = append(out.MentalModels, api.BankTemplateMentalModel{
				ID:          m.ID,
				Name:        m.Name,
				SourceQuery: m.SourceQuery,
				Tags:        m.Tags,
				MaxTokens:   api.OptInt{Set: m.MaxTokens > 0, Value: m.MaxTokens},
			})
		}
	}
	if ds, ok := e.store.(DirectiveSource); ok {
		list, err := ds.ListDirectives(ctx, params.BankID)
		if err != nil {
			return nil, internal(err)
		}
		for _, d := range list {
			out.Directives = append(out.Directives, api.BankTemplateDirective{Name: d.Name, Content: d.Content})
		}
	}
	return out, nil
}

// ImportBankTemplate implements import_bank_template (POST /banks/{bank_id}/import).
// The bank is created when missing; config is applied as per-bank overrides,
// mental models are matched by id, directives by name.
func (e *Engine) ImportBankTemplate(ctx context.Context, req *api.BankTemplateManifest, params api.ImportBankTemplateParams) (api.ImportBankTemplateRes, error) {
	if req == nil {
		return nil, badRequest("Template schema validation failed: a manifest is required")
	}
	if errs := validateBankTemplate(req); len(errs) > 0 {
		return nil, badRequest("Template validation failed: %s", strings.Join(errs, "; "))
	}
	dryRun := params.DryRun.Or(false)

	configUpdates := map[string]any{}
	if req.Bank.Set {
		configUpdates = bankTemplateConfigToMap(req.Bank.Value)
	}
	if dryRun {
		created := make([]string, 0, len(req.MentalModels))
		names := make([]string, 0, len(req.Directives))
		for _, m := range req.MentalModels {
			created = append(created, m.ID)
		}
		for _, d := range req.Directives {
			names = append(names, d.Name)
		}
		return &api.BankTemplateImportResponse{
			BankID:              params.BankID,
			ConfigApplied:       len(configUpdates) > 0,
			MentalModelsCreated: created,
			DirectivesCreated:   names,
			DryRun:              api.OptBool{Set: true, Value: true},
		}, nil
	}

	if _, err := e.store.EnsureBank(ctx, params.BankID, "", ""); err != nil {
		return nil, internal(err)
	}
	if len(configUpdates) > 0 {
		cfgSrc, ok := e.store.(BankConfigSource)
		if !ok {
			return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support bank configuration"}}
		}
		if err := cfgSrc.UpdateBankConfig(ctx, params.BankID, configUpdates); err != nil {
			return nil, internal(err)
		}
	}

	resp := &api.BankTemplateImportResponse{
		BankID:              params.BankID,
		ConfigApplied:       len(configUpdates) > 0,
		MentalModelsCreated: []string{},
		MentalModelsUpdated: []string{},
		DirectivesCreated:   []string{},
		DirectivesUpdated:   []string{},
		OperationIds:        []string{},
		DryRun:              api.OptBool{Set: true, Value: false},
	}

	if len(req.MentalModels) > 0 {
		mms, err := e.mmStore()
		if err != nil {
			return nil, err
		}
		existing, err := mms.ListMentalModels(ctx, params.BankID)
		if err != nil {
			return nil, internal(err)
		}
		byID := map[string]*model.MentalModel{}
		for _, m := range existing {
			byID[m.ID] = m
		}
		for _, row := range req.MentalModels {
			if _, ok := byID[row.ID]; ok {
				updated, err := mms.UpdateMentalModel(ctx, params.BankID, row.ID, func(m *model.MentalModel) {
					m.Name = row.Name
					m.SourceQuery = row.SourceQuery
					m.Tags = row.Tags
					if row.MaxTokens.Set {
						m.MaxTokens = row.MaxTokens.Value
					}
				})
				if err != nil {
					return nil, internal(err)
				}
				if e.provider != nil {
					e.refreshMentalModelNow(ctx, mms, params.BankID, updated)
				}
				resp.MentalModelsUpdated = append(resp.MentalModelsUpdated, row.ID)
				continue
			}
			created, err := mms.CreateMentalModel(ctx, params.BankID, &model.MentalModel{
				ID:          row.ID,
				Name:        row.Name,
				SourceQuery: row.SourceQuery,
				Tags:        row.Tags,
				MaxTokens:   row.MaxTokens.Value,
			})
			if err != nil {
				return nil, internal(err)
			}
			if e.provider != nil {
				e.refreshMentalModelNow(ctx, mms, params.BankID, created)
			}
			resp.MentalModelsCreated = append(resp.MentalModelsCreated, row.ID)
		}
	}

	if len(req.Directives) > 0 {
		ds, ok := e.store.(DirectiveSource)
		if !ok {
			return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support directives"}}
		}
		existing, err := ds.ListDirectives(ctx, params.BankID)
		if err != nil {
			return nil, internal(err)
		}
		byName := map[string]bool{}
		for _, d := range existing {
			byName[d.Name] = true
		}
		updater, canUpdate := e.store.(DirectiveUpdateSource)
		for _, d := range req.Directives {
			if byName[d.Name] {
				if canUpdate {
					if err := updater.UpdateDirective(ctx, params.BankID, d.Name, d.Content); err != nil {
						return nil, internal(err)
					}
				} else if _, err := ds.CreateDirective(ctx, params.BankID, d.Name, d.Content); err != nil {
					return nil, internal(err)
				}
				resp.DirectivesUpdated = append(resp.DirectivesUpdated, d.Name)
				continue
			}
			if _, err := ds.CreateDirective(ctx, params.BankID, d.Name, d.Content); err != nil {
				return nil, internal(err)
			}
			resp.DirectivesCreated = append(resp.DirectivesCreated, d.Name)
		}
	}
	return resp, nil
}

// DirectiveUpdateSource is the extra slice template import needs to update a
// directive whose name already exists.
type DirectiveUpdateSource interface {
	UpdateDirective(ctx context.Context, bankID, name, content string) error
}

// validateBankTemplate mirrors the semantic checks upstream runs after the
// structural parse.
func validateBankTemplate(m *api.BankTemplateManifest) []string {
	var errs []string
	version := strings.TrimSpace(m.Version)
	if version == "" {
		errs = append(errs, "version: field required")
	} else {
		n := 0
		if _, err := fmt.Sscanf(version, "%d", &n); err != nil || n < 1 {
			errs = append(errs, fmt.Sprintf("version must be a numeric string, got '%s'", version))
		} else if n > 1 {
			errs = append(errs, fmt.Sprintf("version '%s' is not supported by this server (max supported: 1). Please upgrade Hindsight.", version))
		}
	}
	if m.Bank.Set {
		cfg := m.Bank.Value
		if cfg.RetainExtractionMode.Set {
			switch cfg.RetainExtractionMode.Value {
			case "concise", "verbose", "custom", "verbatim", "chunks":
			default:
				errs = append(errs, fmt.Sprintf("bank.retain_extraction_mode: must be one of ('concise', 'verbose', 'custom', 'verbatim', 'chunks'), got '%s'", cfg.RetainExtractionMode.Value))
			}
		}
		if cfg.RetainCustomInstructions.Set && cfg.RetainCustomInstructions.Value != "" &&
			(!cfg.RetainExtractionMode.Set || cfg.RetainExtractionMode.Value != "custom") {
			errs = append(errs, "bank.retain_custom_instructions: requires retain_extraction_mode='custom'")
		}
	}
	seenModels := map[string]bool{}
	for i, mm := range m.MentalModels {
		if strings.TrimSpace(mm.ID) == "" {
			errs = append(errs, fmt.Sprintf("mental_models[%d].id: must not be empty", i))
		}
		if strings.TrimSpace(mm.Name) == "" {
			errs = append(errs, fmt.Sprintf("mental_models[%d].name: must not be empty", i))
		}
		if strings.TrimSpace(mm.SourceQuery) == "" {
			errs = append(errs, fmt.Sprintf("mental_models[%d].source_query: must not be empty", i))
		}
		if seenModels[mm.ID] {
			errs = append(errs, fmt.Sprintf("mental_models[%d].id: duplicate id '%s'", i, mm.ID))
		}
		seenModels[mm.ID] = true
	}
	seenDirectives := map[string]bool{}
	for i, d := range m.Directives {
		if strings.TrimSpace(d.Name) == "" {
			errs = append(errs, fmt.Sprintf("directives[%d].name: must not be empty", i))
		}
		if strings.TrimSpace(d.Content) == "" {
			errs = append(errs, fmt.Sprintf("directives[%d].content: must not be empty", i))
		}
		if seenDirectives[d.Name] {
			errs = append(errs, fmt.Sprintf("directives[%d].name: duplicate name '%s'", i, d.Name))
		}
		seenDirectives[d.Name] = true
	}
	return errs
}

// jsonByName maps a JSON field name to its snake_case form for config rows.
func bankTemplateConfigToMap(cfg api.BankTemplateConfig) map[string]any {
	out := map[string]any{}
	// Marshal through the pointer: the generated struct's MarshalJSON (and the
	// field-skipping it performs) is only reachable that way, and the Opt*
	// field marshalers emit nothing for unset values on their own.
	raw, err := json.Marshal(&cfg)
	if err != nil {
		return out
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return out
	}
	for k, v := range decoded {
		if v == nil {
			continue
		}
		out[k] = v
	}
	return out
}

// bankTemplateConfigFromMap fills an api.BankTemplateConfig from stored
// overrides, dropping keys the template model does not know.
func bankTemplateConfigFromMap(m map[string]any) api.BankTemplateConfig {
	raw, err := json.Marshal(m)
	if err != nil {
		return api.BankTemplateConfig{}
	}
	var cfg api.BankTemplateConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return api.BankTemplateConfig{}
	}
	return cfg
}

// CloneBank implements clone_bank (POST /banks/{bank_id}/clone): the export
// and the restore run back to back on this instance, so nothing is
// re-extracted and no LLM is called.
func (e *Engine) CloneBank(ctx context.Context, params api.CloneBankParams) (api.CloneBankRes, error) {
	target := strings.TrimSpace(params.TargetBankID)
	if target == "" {
		return nil, badRequest("target_bank_id is required")
	}
	if target == params.BankID {
		return nil, badRequest("Bank '%s' cannot be cloned onto itself", params.BankID)
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("Bank '%s' not found", params.BankID)
	}
	if existing, err := e.store.GetBank(ctx, target); err != nil {
		return nil, internal(err)
	} else if existing != nil {
		return nil, badRequest("Target bank '%s' already exists; a clone needs a fresh bank id", target)
	}
	includeData := params.IncludeData.Or(true)
	includeConfig := params.IncludeBankConfig.Or(true)
	includeHistory := params.IncludeHistory.Or(false)
	if !includeData && !includeConfig && !includeHistory {
		return nil, badRequest("Nothing to clone: set at least one of include_data, include_bank_config, include_history")
	}

	archive, err := e.buildTransferArchive(ctx, params.BankID, nil, transferOptions{
		ArchiveType:   transfer.ArchiveBank,
		Observations:  includeData,
		BankConfig:    includeConfig,
		KnowledgeBase: includeData,
	})
	if err != nil {
		return nil, err
	}
	manifest, _, err := transfer.Read(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, internal(err)
	}
	sections, err := transfer.ReadSections(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, internal(err)
	}

	if _, err := e.store.EnsureBank(ctx, target, "", ""); err != nil {
		return nil, internal(err)
	}
	if err := e.restoreArchiveSections(ctx, target, sections, includeConfig, includeData); err != nil {
		return nil, err
	}
	restored, skipped := 0, 0
	if includeData {
		if restored, skipped, err = e.restoreArchiveDocumentsWithSections(ctx, target, archive, manifest, sections); err != nil {
			return nil, err
		}
	}
	opID := recordOperation(params.BankID, "clone_bank", map[string]any{
		"target_bank_id": target, "documents_imported": restored, "documents_skipped": skipped,
	})
	return &api.BankTransferSubmitResponse{OperationID: opID, Status: api.OptString{Set: true, Value: "completed"}}, nil
}
