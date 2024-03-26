package main

import (
	"context"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"github.com/highlight-run/highlight/backend/model"
)

func main() {
	ctx := context.Background()

	db, err := model.SetupDB(ctx, os.Getenv("PSQL_DB"))
	if err != nil {
		log.WithContext(ctx).Fatal(err)
	}

	if err := db.Exec(`
		CREATE TABLE IF NOT EXISTS migrated_embeddings (
			project_id INTEGER PRIMARY KEY NOT NULL,
			embedding_id INTEGER NOT NULL
		)`).Error; err != nil {
		log.WithContext(ctx).Fatal(err)
	}

	var lastCreatedPart int
	if err := db.Raw("select split_part(relname, '_', 5) from pg_stat_all_tables where relname like 'error_object_embeddings_partitioned%' order by relid desc limit 1").
		Scan(&lastCreatedPart).Error; err != nil {
		log.WithContext(ctx).Fatal(err)
	}

	// Only running this migration on project_id = 1 for now
	for i := 1; i <= 1; i++ {
		log.WithContext(ctx).Infof("beginning loop: %d", i)
		tablename := fmt.Sprintf("error_object_embeddings_partitioned_%d", i)

		if err := db.Exec(fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_%s_id ON %s (id)", tablename, tablename)).Error; err != nil {
			log.WithContext(ctx).Fatal(err)
		}
		log.WithContext(ctx).Info("done creating index")

		var prevEmbeddingId int
		if err := db.Raw("select coalesce(max(embedding_id), 0) from migrated_embeddings where project_id = ?", i).Scan(&prevEmbeddingId).Error; err != nil {
			log.WithContext(ctx).Fatal(err)
		}
		log.WithContext(ctx).Infof("prevEmbeddingId: %d", prevEmbeddingId)

		var maxEmbeddingId int
		if err := db.Raw("select coalesce(max(id), 0) from error_object_embeddings_partitioned eoe where project_id = ?", i).Scan(&maxEmbeddingId).Error; err != nil {
			log.WithContext(ctx).Fatal(err)
		}
		log.WithContext(ctx).Infof("maxEmbeddingId: %d", maxEmbeddingId)

		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`
				insert into error_group_embeddings (project_id, error_group_id, count, gte_large_embedding)
				select a.* from (
					select eo.project_id, eo.error_group_id, count(*) as count, AVG(eoe.gte_large_embedding) as gte_large_embedding
					from error_object_embeddings_partitioned eoe
					inner join error_objects eo
					on eoe.error_object_id = eo.id
					where eoe.gte_large_embedding is not null
					and eoe.id > ?
					and eoe.id <= ?
					group by eo.project_id, eo.error_group_id) a
				on conflict (project_id, error_group_id)
				do update set
					gte_large_embedding = 
						error_group_embeddings.gte_large_embedding * array_fill(error_group_embeddings.count::numeric / (error_group_embeddings.count + excluded.count), '{1024}')::vector
						+ excluded.gte_large_embedding * array_fill(excluded.count::numeric / (error_group_embeddings.count + excluded.count), '{1024}')::vector,
					count = error_group_embeddings.count + excluded.count
			`, prevEmbeddingId, maxEmbeddingId).Error; err != nil {
				return err
			}

			log.WithContext(ctx).Info("done upserting new embeddings")

			if err := tx.Exec(`
				insert into migrated_embeddings (project_id, embedding_id)
				values (?, ?)
				on conflict (project_id)
				do update set embedding_id = excluded.embedding_id
			`, i, maxEmbeddingId).Error; err != nil {
				return err
			}

			log.WithContext(ctx).Info("done updating maxEmbeddingId")

			return nil
		}); err != nil {
			log.WithContext(ctx).Fatal(err)
		}
		log.WithContext(ctx).Infof("done loop: %d", i)
	}

}