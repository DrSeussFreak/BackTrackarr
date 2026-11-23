package store

import "context"

// JobRecord is a background job saved so it can continue after a restart.
type JobRecord struct {
	ID, Kind, Name, State, Message, Err string
	Params, Checkpoint                  []byte
	Created, Updated                    int64
	Done, Total                         int
}

// SaveJob inserts or updates a job.
func (s *Store) SaveJob(ctx context.Context, r JobRecord) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO jobs(id,kind,name,state,message,err,params,checkpoint,created,updated,done,total)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET state=excluded.state, message=excluded.message, err=excluded.err,
		 checkpoint=excluded.checkpoint, updated=excluded.updated, done=excluded.done, total=excluded.total`,
		r.ID, r.Kind, r.Name, r.State, r.Message, r.Err, string(r.Params), string(r.Checkpoint), r.Created, r.Updated, r.Done, r.Total)
	return err
}

// ListJobs returns every saved job, oldest first.
func (s *Store) ListJobs(ctx context.Context) ([]JobRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,kind,name,state,message,err,params,checkpoint,created,updated,done,total FROM jobs ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRecord
	for rows.Next() {
		var r JobRecord
		var params, checkpoint string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Name, &r.State, &r.Message, &r.Err, &params, &checkpoint, &r.Created, &r.Updated, &r.Done, &r.Total); err != nil {
			return nil, err
		}
		r.Params, r.Checkpoint = []byte(params), []byte(checkpoint)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteJob removes a saved job.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM jobs WHERE id=?`, id)
	return err
}
