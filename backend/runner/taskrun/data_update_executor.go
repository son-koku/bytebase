package taskrun

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/pkg/errors"

	storepb "github.com/bytebase/bytebase/proto/generated-go/store"
	v1pb "github.com/bytebase/bytebase/proto/generated-go/v1"

	"github.com/bytebase/bytebase/backend/common"
	"github.com/bytebase/bytebase/backend/common/log"
	"github.com/bytebase/bytebase/backend/component/activity"
	"github.com/bytebase/bytebase/backend/component/config"
	"github.com/bytebase/bytebase/backend/component/dbfactory"
	"github.com/bytebase/bytebase/backend/component/state"
	enterprise "github.com/bytebase/bytebase/backend/enterprise/api"
	"github.com/bytebase/bytebase/backend/runner/schemasync"

	api "github.com/bytebase/bytebase/backend/legacyapi"
	"github.com/bytebase/bytebase/backend/plugin/db"
	"github.com/bytebase/bytebase/backend/plugin/parser/base"
	"github.com/bytebase/bytebase/backend/store"
	"github.com/bytebase/bytebase/backend/store/model"
)

// NewDataUpdateExecutor creates a data update (DML) task executor.
func NewDataUpdateExecutor(store *store.Store, dbFactory *dbfactory.DBFactory, activityManager *activity.Manager, license enterprise.LicenseService, stateCfg *state.State, schemaSyncer *schemasync.Syncer, profile config.Profile) Executor {
	return &DataUpdateExecutor{
		store:           store,
		dbFactory:       dbFactory,
		activityManager: activityManager,
		license:         license,
		stateCfg:        stateCfg,
		schemaSyncer:    schemaSyncer,
		profile:         profile,
	}
}

// DataUpdateExecutor is the data update (DML) task executor.
type DataUpdateExecutor struct {
	store           *store.Store
	dbFactory       *dbfactory.DBFactory
	activityManager *activity.Manager
	license         enterprise.LicenseService
	stateCfg        *state.State
	schemaSyncer    *schemasync.Syncer
	profile         config.Profile
}

// RunOnce will run the data update (DML) task executor once.
func (exec *DataUpdateExecutor) RunOnce(ctx context.Context, driverCtx context.Context, task *store.TaskMessage, taskRunUID int) (terminated bool, result *storepb.TaskRunResult, err error) {
	exec.stateCfg.TaskRunExecutionStatuses.Store(taskRunUID,
		state.TaskRunExecutionStatus{
			ExecutionStatus: v1pb.TaskRun_PRE_EXECUTING,
			UpdateTime:      time.Now(),
		})

	payload := &api.TaskDatabaseDataUpdatePayload{}
	if err := json.Unmarshal([]byte(task.Payload), payload); err != nil {
		return true, nil, errors.Wrap(err, "invalid database data update payload")
	}

	statement, err := exec.store.GetSheetStatementByID(ctx, payload.SheetID)
	if err != nil {
		return true, nil, err
	}
	if err := exec.backupData(ctx, driverCtx, statement, payload, task); err != nil {
		return true, nil, err
	}
	version := model.Version{Version: payload.SchemaVersion}
	return runMigration(ctx, driverCtx, exec.store, exec.dbFactory, exec.stateCfg, exec.profile, task, taskRunUID, db.Data, statement, version, &payload.SheetID)
}

func (exec *DataUpdateExecutor) backupData(
	ctx context.Context,
	driverCtx context.Context,
	statement string,
	payload *api.TaskDatabaseDataUpdatePayload,
	task *store.TaskMessage,
) error {
	if payload.PreUpdateBackupDetail.Database == "" {
		return nil
	}

	instance, err := exec.store.GetInstanceV2(ctx, &store.FindInstanceMessage{UID: &task.InstanceID})
	if err != nil {
		return err
	}
	database, err := exec.store.GetDatabaseV2(ctx, &store.FindDatabaseMessage{UID: task.DatabaseID})
	if err != nil {
		return err
	}
	issue, err := exec.store.GetIssueV2(ctx, &store.FindIssueMessage{PipelineID: &task.PipelineID})
	if err != nil {
		return errors.Wrapf(err, "failed to find issue for pipeline %v", task.PipelineID)
	}
	if issue == nil {
		return errors.Errorf("issue not found for pipeline %v", task.PipelineID)
	}

	backupInstanceID, backupDatabaseName, err := common.GetInstanceDatabaseID(payload.PreUpdateBackupDetail.Database)
	if err != nil {
		return err
	}
	backupDatabase, err := exec.store.GetDatabaseV2(ctx, &store.FindDatabaseMessage{InstanceID: &backupInstanceID, DatabaseName: &backupDatabaseName})
	if err != nil {
		return err
	}
	if backupDatabase == nil {
		return errors.Errorf("backup database %q not found", payload.PreUpdateBackupDetail.Database)
	}

	driver, err := exec.dbFactory.GetAdminDatabaseDriver(driverCtx, instance, database, db.ConnectionContext{})
	if err != nil {
		return err
	}
	defer driver.Close(driverCtx)

	backupDriver, err := exec.dbFactory.GetAdminDatabaseDriver(driverCtx, instance, backupDatabase, db.ConnectionContext{})
	if err != nil {
		return err
	}
	defer backupDriver.Close(driverCtx)

	prefix := "_" + time.Now().Format("20060102150405")
	statements, err := base.TransformDMLToSelect(instance.Engine, statement, database.DatabaseName, backupDatabaseName, prefix)
	if err != nil {
		return errors.Wrap(err, "failed to transform DML to select")
	}

	for _, statement := range statements {
		if _, err := driver.Execute(driverCtx, statement.Statement, db.ExecuteOptions{}); err != nil {
			return err
		}
		var originalLine *int32
		switch instance.Engine {
		case storepb.Engine_MYSQL, storepb.Engine_TIDB:
			if _, err := driver.Execute(driverCtx, fmt.Sprintf("ALTER TABLE `%s`.`%s` COMMENT = 'issue %d'", backupDatabaseName, statement.TableName, issue.UID), db.ExecuteOptions{}); err != nil {
				return err
			}
		case storepb.Engine_MSSQL:
			if _, err := backupDriver.Execute(driverCtx, fmt.Sprintf("EXEC sp_addextendedproperty 'MS_Description', 'issue %d', 'SCHEMA', 'dbo', 'TABLE', '%s'", issue.UID, statement.TableName), db.ExecuteOptions{}); err != nil {
				return err
			}
			num := int32(statement.OriginalLine)
			originalLine = &num
		}

		if err := exec.store.CreateIssueComment(ctx, &store.IssueCommentMessage{
			IssueUID: issue.UID,
			Payload: &storepb.IssueCommentPayload{
				Event: &storepb.IssueCommentPayload_TaskPriorBackup_{
					TaskPriorBackup: &storepb.IssueCommentPayload_TaskPriorBackup{
						Task:     common.FormatTask(issue.Project.ResourceID, task.PipelineID, task.StageID, task.ID),
						Database: backupDatabaseName,
						Tables: []*storepb.IssueCommentPayload_TaskPriorBackup_Table{
							{
								Schema: "",
								Table:  statement.TableName,
							},
						},
						OriginalLine: originalLine,
					},
				},
			},
		}, api.SystemBotID); err != nil {
			slog.Warn("failed to create issue comment", "task", task.ID, log.BBError(err))
		}
	}

	if err := exec.schemaSyncer.SyncDatabaseSchema(ctx, backupDatabase, true /* force */); err != nil {
		slog.Error("failed to sync backup database schema",
			slog.String("database", payload.PreUpdateBackupDetail.Database),
			log.BBError(err),
		)
	}
	return nil
}
