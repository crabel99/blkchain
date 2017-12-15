package blkchain

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Explanation of how we handle integers. In Bitcoin structures most
// integers are uint32. Postgres does not have an unsigned int type,
// but using a bigint to store integers seems like a waste of
// space. So we cast all uints to int32, and thus 0xFFFFFFFF would
// become -1 in Postgres, which is fine as long as we know all the
// bits are correct.

var writerWg sync.WaitGroup

type blockRec struct {
	id      int
	height  int
	block   *Block
	hash    Uint256
	orphan  bool
	status  int
	filen   int
	filepos int
	sync    chan bool
}

type txRec struct {
	id      int64
	blockId int
	n       int // position within block
	tx      *Tx
	hash    Uint256
	sync    chan bool
	dupe    bool // already seen
}

type txInRec struct {
	txId    int64
	n       int
	txIn    *TxIn
	idCache *txIdCache
}

type txOutRec struct {
	txId  int64
	n     int
	txOut *TxOut
}

type BlockInfo struct {
	*Block
	Height,
	Status,
	FileN,
	FilePos int
}

type PGWriter struct {
	blockCh chan *BlockInfo
	utxoCh  chan *UTXO
	wg      *sync.WaitGroup
	db      *sql.DB
}

func NewPGWriter(connstr string, cacheSize int) (*PGWriter, error) {

	var wg sync.WaitGroup

	db, err := sql.Open("postgres", connstr)
	if err != nil {
		return nil, err
	}

	deferredIndexes := true
	if err := createTables(db); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			// this is fine, cancel deferred index/constraint creation
			deferredIndexes = false
		} else {
			log.Printf("Tables created without indexes, which are created at the very end.")
			return nil, err
		}
	}

	bch := make(chan *BlockInfo, 64)
	wg.Add(1)
	go pgBlockWorker(bch, &wg, db, deferredIndexes, cacheSize)

	uch := make(chan *UTXO, 64)
	wg.Add(1)
	go pgUTXOWriter(uch, &wg, db)

	return &PGWriter{
		blockCh: bch,
		utxoCh:  uch,
		wg:      &wg,
		db:      db,
	}, nil
}

func (p *PGWriter) Close() {
	close(p.blockCh)
	close(p.utxoCh)
	p.wg.Wait()
}

func (p *PGWriter) WriteBlockInfo(b *BlockInfo) {
	p.blockCh <- b
}

func (p *PGWriter) WriteUTXO(u *UTXO) {
	p.utxoCh <- u
}

func (w *PGWriter) LastHeight() (int, error) {
	_, height, _, err := getLastHashAndHeight(w.db)
	return height, err
}

func pgBlockWorker(ch <-chan *BlockInfo, wg *sync.WaitGroup, db *sql.DB, deferredIndexes bool, cacheSize int) {
	defer wg.Done()

	bid, _, bhash, err := getLastHashAndHeight(db)
	if err != nil {
		log.Printf("Error getting last hash and height, exiting: %v", err)
		return
	}
	txid, err := getLastTxId(db)
	if err != nil {
		log.Printf("Error getting last tx id, exiting: %v", err)
		return
	}

	blockCh := make(chan *blockRec, 64)
	go pgBlockWriter(blockCh, db)

	txCh := make(chan *txRec, 64)
	go pgTxWriter(txCh, db)

	txInCh := make(chan *txInRec, 64)
	go pgTxInWriter(txInCh, db)

	txOutCh := make(chan *txOutRec, 64)
	go pgTxOutWriter(txOutCh, db)

	writerWg.Add(4)

	start := time.Now()

	if len(bhash) > 0 {
		log.Printf("Skipping to hash %v", uint256FromBytes(bhash))
		skip, last := 0, time.Now()
		for b := range ch {
			hash := b.Hash()
			if bytes.Compare(bhash, hash[:]) == 0 {
				break
			} else {
				skip++
				if skip%10 == 0 && time.Now().Sub(last) > 5*time.Second {
					log.Printf("Skipped %d blocks...", skip)
					last = time.Now()
				}
			}
		}
		log.Printf("Skipped %d total blocks.", skip)
	}

	idCache := newTxIdCache(cacheSize)

	var syncCh chan bool
	if !deferredIndexes {
		// no deferredIndexes means that the constraints already
		// exist, and we need to wait for a tx to be commited before
		// ins/outs can be inserted. Same with block/tx.
		syncCh = make(chan bool, 0)
	}

	txcnt, last := 0, time.Now()
	for bi := range ch {
		bid++
		hash := bi.Hash()
		blockCh <- &blockRec{
			id:      bid,
			height:  bi.Height,
			block:   bi.Block,
			hash:    hash,
			status:  bi.Status,
			filen:   bi.FileN,
			filepos: bi.FilePos,
			sync:    syncCh,
		}
		if syncCh != nil {
			<-syncCh
		}

		for n, tx := range bi.Txs {
			txid++
			txcnt++

			hash := tx.Hash()

			// Check if recently seen and add to cache.
			recentId := idCache.add(hash, txid, len(tx.TxOuts))
			txCh <- &txRec{
				id:      recentId,
				n:       n,
				blockId: bid,
				tx:      tx,
				hash:    hash,
				sync:    syncCh,
				dupe:    recentId != txid,
			}

			if syncCh != nil {
				<-syncCh
			}

			if recentId != txid {
				// This is a recent transaction, nothing to do
				continue
			}

			for n, txin := range tx.TxIns {
				txInCh <- &txInRec{
					txId:    txid,
					n:       n,
					txIn:    txin,
					idCache: idCache,
				}
			}

			for n, txout := range tx.TxOuts {
				txOutCh <- &txOutRec{
					txId:  txid,
					n:     n,
					txOut: txout,
				}
			}
		}

		if !deferredIndexes {
			// commit after every block
			// blocks and txs are already commited
			txInCh <- nil
			txOutCh <- nil
		} else if bid%50 == 0 {
			// commit every N blocks
			blockCh <- nil
			txCh <- nil
			txInCh <- nil
			txOutCh <- nil
		}

		// report progress
		if time.Now().Sub(last) > 5*time.Second {
			log.Printf("Height: %d Txs: %d Time: %v Tx/s: %02f",
				bi.Height, txcnt, time.Unix(int64(bi.Time), 0), float64(txcnt)/time.Now().Sub(start).Seconds())
			last = time.Now()
		}
	}

	close(blockCh)
	close(txInCh)
	close(txOutCh)
	close(txCh)

	log.Printf("Closed db channels, waiting for workers to finish...")
	writerWg.Wait()
	log.Printf("Workers finished.")

	log.Printf("Txid cache hits: %d (%.02f%%) misses: %d collisions: %d dupes: %d evictions: %d",
		idCache.hits, float64(idCache.hits)/(float64(idCache.hits+idCache.miss)+0.0001)*100,
		idCache.miss, idCache.cols, idCache.dups, idCache.evic)

	verbose := deferredIndexes
	log.Printf("Creating indexes part 1 (if needed), please be patient, this may take a long time...")
	if err := createIndexes1(db, verbose); err != nil {
		log.Printf("Error creating indexes: %v", err)
	}
	if idCache.miss > 0 {
		log.Printf("Fixing missing prevout_tx_id entries (if needed), this may take a long time...")
		if err := fixPrevoutTxId(db); err != nil {
			log.Printf("Error fixing prevout_tx_id: %v", err)
		}
	} else {
		log.Printf("NOT fixing missing prevout_tx_id entries because there were 0 cache misses.")

	}
	// log.Printf("Marking spent outputs (if needed), this may take a *really* long time (hours)...")
	// if err := markSpentOutputs(db); err != nil {
	// 	log.Printf("Error fixing prevout_tx_id: %v", err)
	// }
	log.Printf("Linking UTXOs (if needed), this may take a long time...")
	if err := linkUTXOs(db); err != nil {
		log.Printf("Error linking utxos: %v", err)
	}
	log.Printf("Creating indexes part 2 (if needed), please be patient, this may take a long time...")
	if err := createIndexes2(db, verbose); err != nil {
		log.Printf("Error creating indexes: %v", err)
	}
	log.Printf("Creating constraints (if needed), please be patient, this may take a long time...")
	if err := createConstraints(db, verbose); err != nil {
		log.Printf("Error creating constraints: %v", err)
	}
	log.Printf("Marking orphan blocks...")
	if err := setOrphans(db, 0); err != nil {
		log.Printf("Error marking orphans: %v", err)
	}
	log.Printf("Indexes and constraints created.")
}

func begin(db *sql.DB, table string, cols []string) (*sql.Tx, *sql.Stmt, error) {
	txn, err := db.Begin()
	if err != nil {
		return nil, nil, err
	}

	stmt, err := txn.Prepare(pq.CopyIn(table, cols...))
	if err != nil {
		return nil, nil, err
	}
	return txn, stmt, nil
}

func pgBlockWriter(c chan *blockRec, db *sql.DB) {
	defer writerWg.Done()

	cols := []string{"id", "height", "hash", "version", "prevhash", "merkleroot", "time", "bits", "nonce", "orphan", "status", "filen", "filepos"}

	txn, stmt, err := begin(db, "blocks", cols)
	if err != nil {
		log.Printf("ERROR (1): %v", err)
	}

	for br := range c {

		if br == nil { // commit signal
			if err = commit(stmt, txn); err != nil {
				log.Printf("Block commit error: %v", err)
			}
			txn, stmt, err = begin(db, "blocks", cols)
			if err != nil {
				log.Printf("ERROR (2): %v", err)
			}
			continue
		}

		b := br.block
		_, err = stmt.Exec(
			br.id,
			br.height,
			br.hash[:],
			int32(b.Version),
			b.PrevHash[:],
			b.HashMerkleRoot[:],
			int32(b.Time),
			int32(b.Bits),
			int32(b.Nonce),
			br.orphan,
			int32(br.status),
			int32(br.filen),
			int32(br.filepos),
		)
		if err != nil {
			log.Printf("ERROR (3): %v", err)
		}

		if br.sync != nil {
			// commit and send confirmation
			if err = commit(stmt, txn); err != nil {
				log.Printf("Block commit error (2): %v", err)
			}
			txn, stmt, err = begin(db, "blocks", cols)
			if err != nil {
				log.Printf("ERROR (2.5): %v", err)
			}
			br.sync <- true
		}

	}

	log.Printf("Block writer channel closed, leaving.")
	if err = commit(stmt, txn); err != nil {
		log.Printf("Block commit error: %v", err)
	}

}

func pgTxWriter(c chan *txRec, db *sql.DB) {
	defer writerWg.Done()

	cols := []string{"id", "txid", "version", "locktime"}
	bcols := []string{"block_id", "n", "tx_id"}

	txn, stmt, err := begin(db, "txs", cols)
	if err != nil {
		log.Printf("ERROR (3): %v", err)
	}

	btxn, bstmt, err := begin(db, "block_txs", bcols)
	if err != nil {
		log.Printf("ERROR (4): %v", err)
	}

	for tr := range c {
		if tr == nil { // commit signal
			if err = commit(stmt, txn); err != nil {
				log.Printf("Tx commit error: %v", err)
			}
			if err = commit(bstmt, btxn); err != nil {
				log.Printf("Block Txs commit error: %v", err)
			}
			txn, stmt, err = begin(db, "txs", cols)
			if err != nil {
				log.Printf("ERROR (5): %v", err)
			}
			btxn, bstmt, err = begin(db, "block_txs", bcols)
			if err != nil {
				log.Printf("ERROR (6): %v", err)
			}
			continue
		}

		if !tr.dupe {
			t := tr.tx
			_, err = stmt.Exec(
				tr.id,
				tr.hash[:],
				int32(t.Version),
				int32(t.LockTime),
			)
			if err != nil {
				log.Printf("ERROR (7): %v", err)
			}
			// It can still be a dupe if we are catching up and the
			// cache is empty. In which case we will get a Tx commit
			// error below, which is fine.
		}

		_, err = bstmt.Exec(
			tr.blockId,
			tr.n,
			tr.id,
		)
		if err != nil {
			log.Printf("ERROR (7.5): %v", err)
		}

		if tr.sync != nil {
			// commit and send confirmation
			if err = commit(stmt, txn); err != nil {
				log.Printf("Tx commit error: %v", err)
			}
			if err = commit(bstmt, btxn); err != nil {
				log.Printf("Block Txs commit error: %v", err)
			}
			txn, stmt, err = begin(db, "txs", cols)
			if err != nil {
				log.Printf("ERROR (8): %v", err)
			}
			btxn, bstmt, err = begin(db, "block_txs", bcols)
			if err != nil {
				log.Printf("ERROR (8.5): %v", err)
			}
			tr.sync <- true
		}
	}

	log.Printf("Tx writer channel closed, leaving.")
	if err = commit(stmt, txn); err != nil {
		log.Printf("Tx commit error: %v", err)
	}
	if err = commit(bstmt, btxn); err != nil {
		log.Printf("Block Txs commit error: %v", err)
	}

}

func pgTxInWriter(c chan *txInRec, db *sql.DB) {
	defer writerWg.Done()

	cols := []string{"tx_id", "n", "prevout_hash", "prevout_n", "scriptsig", "sequence", "witness", "prevout_tx_id"}

	txn, stmt, err := begin(db, "txins", cols)
	if err != nil {
		log.Printf("ERROR (9): %v", err)
	}

	for tr := range c {
		if tr == nil { // commit signal
			if err = commit(stmt, txn); err != nil {
				log.Printf("Txin commit error: %v", err)
			}
			txn, stmt, err = begin(db, "txins", cols)
			if err != nil {
				log.Printf("ERROR (10): %v", err)
			}
			continue
		}

		t := tr.txIn
		var wb interface{}
		if t.Witness != nil {
			var b bytes.Buffer
			BinWrite(&t.Witness, &b)
			wb = b.Bytes()
		}

		var prevOutTxId *int64 = nil
		if t.PrevOut.N != 0xffffffff { // coinbase
			prevOutTxId = tr.idCache.check(t.PrevOut.Hash)
		}

		_, err = stmt.Exec(
			tr.txId,
			tr.n,
			t.PrevOut.Hash[:],
			int32(t.PrevOut.N),
			t.ScriptSig,
			int32(t.Sequence),
			wb,
			prevOutTxId,
		)
		if err != nil {
			log.Printf("ERROR (11): %v", err)
		}

	}

	log.Printf("TxIn writer channel closed, leaving.")
	if err = commit(stmt, txn); err != nil {
		log.Printf("TxIn commit error: %v", err)
	}
}

func pgTxOutWriter(c chan *txOutRec, db *sql.DB) {
	defer writerWg.Done()

	cols := []string{"tx_id", "n", "value", "scriptpubkey"}

	txn, stmt, err := begin(db, "txouts", cols)
	if err != nil {
		log.Printf("ERROR (12): %v", err)
	}

	for tr := range c {

		if tr == nil { // commit signal
			if err = commit(stmt, txn); err != nil {
				log.Printf("TxOut commit error: %v", err)
			}
			txn, stmt, err = begin(db, "txouts", cols)
			if err != nil {
				log.Printf("ERROR (13): %v", err)
			}
			continue
		}

		t := tr.txOut
		_, err = stmt.Exec(
			tr.txId,
			tr.n,
			t.Value,
			t.ScriptPubKey,
		)
		if err != nil {
			log.Printf("ERROR (11): %v\n", err)
		}

	}

	log.Printf("TxOut writer channel closed, leaving.")
	if err = commit(stmt, txn); err != nil {
		log.Printf("TxOut commit error: %v", err)
	}
}

func pgUTXOWriter(c chan *UTXO, wg *sync.WaitGroup, db *sql.DB) {
	defer wg.Done()

	cols := []string{"txid", "n", "height", "coinbase", "value", "scriptpubkey"}

	txn, stmt, err := begin(db, "utxos", cols)
	if err != nil {
		log.Printf("ERROR (13): %v", err)
	}

	count := 0
	last, start := time.Now(), time.Now()
	for u := range c {

		if u == nil { // commit signal
			if err = commit(stmt, txn); err != nil {
				log.Printf("UTXO commit error: %v", err)
			}
			txn, stmt, err = begin(db, "utxos", cols)
			if err != nil {
				log.Printf("ERROR (14): %v", err)
			}
			continue
		}

		_, err = stmt.Exec(
			u.Hash[:],
			u.N,
			u.Height,
			u.Coinbase,
			u.Value,
			u.ScriptPubKey,
		)
		if err != nil {
			log.Printf("ERROR (15): %v\n", err)
		}
		count++

		// report progress
		if time.Now().Sub(last) > 5*time.Second {
			log.Printf("UTXOs: %d, rows/s: %02f",
				count, float64(count)/time.Now().Sub(start).Seconds())
			last = time.Now()
		}
	}

	log.Printf("UTXO writer channel closed, leaving.")
	if err = commit(stmt, txn); err != nil {
		log.Printf("UTXO commit error: %v", err)
	}
}

func commit(stmt *sql.Stmt, txn *sql.Tx) (err error) {
	_, err = stmt.Exec()
	if err != nil {
		return err
	}
	err = stmt.Close()
	if err != nil {
		return err
	}
	err = txn.Commit()
	if err != nil {
		return err
	}
	return nil
}

func getLastHashAndHeight(db *sql.DB) (int, int, []byte, error) {

	rows, err := db.Query("SELECT id, height, hash FROM blocks ORDER BY height DESC LIMIT 1")
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()

	if rows.Next() {
		var (
			id     int
			height int
			hash   []byte
		)
		if err := rows.Scan(&id, &height, &hash); err != nil {
			return 0, 0, nil, err
		}
		return id, height, hash, nil
	}
	// Initial height is -1, so that 1st block is height 0
	return 0, -1, nil, rows.Err()
}

func getLastTxId(db *sql.DB) (int64, error) {

	rows, err := db.Query("SELECT id FROM txs ORDER BY id DESC LIMIT 1")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	if rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		return id, nil
	}
	return 0, rows.Err()
}

func createTables(db *sql.DB) error {
	sqlTables := `
  CREATE TABLE blocks (
   id           SERIAL
  ,height       INT NOT NULL -- not same as id, because orphans.
  ,hash         BYTEA NOT NULL
  ,version      INT NOT NULL
  ,prevhash     BYTEA NOT NULL
  ,merkleroot   BYTEA NOT NULL
  ,time         INT NOT NULL
  ,bits         INT NOT NULL
  ,nonce        INT NOT NULL
  ,orphan       BOOLEAN NOT NULL DEFAULT false
  ,status       INT NOT NULL
  ,filen        INT NOT NULL
  ,filepos      INT NOT NULL
  );

  CREATE TABLE txs (
   id            BIGSERIAL
  ,txid          BYTEA NOT NULL
  ,version       INT NOT NULL
  ,locktime      INT NOT NULL
  );

  CREATE TABLE block_txs (
   block_id      INT NOT NULL
  ,n             INT NOT NULL
  ,tx_id         BIGINT NOT NULL
  );

  CREATE TABLE txins (
   tx_id         BIGINT NOT NULL
  ,n             INT NOT NULL
  ,prevout_hash  BYTEA NOT NULL
  ,prevout_n     INT NOT NULL
  ,scriptsig     BYTEA NOT NULL
  ,sequence      INT NOT NULL
  ,witness       BYTEA
  ,prevout_tx_id BIGINT
  );

  CREATE TABLE txouts (
   tx_id        BIGINT NOT NULL
  ,n            INT NOT NULL
  ,value        BIGINT NOT NULL
  ,scriptpubkey BYTEA NOT NULL
  );

  CREATE TABLE utxos (
   tx_id        BIGINT         -- NOT NULL
  ,txid         BYTEA NOT NULL
  ,n            INT NOT NULL
  ,height       INT NOT NULL
  ,coinbase     BOOL NOT NULL
  ,value        BIGINT NOT NULL
  ,scriptpubkey BYTEA NOT NULL
  );
`
	_, err := db.Exec(sqlTables)
	return err
}

func createIndexes1(db *sql.DB, verbose bool) error {
	// Adding a constraint or index if it does not exist is a little tricky in PG
	if verbose {
		log.Printf("  - blocks primary key...")
	}
	if _, err := db.Exec(`
       DO $$
       BEGIN
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'blocks' AND constraint_name = 'blocks_pkey') THEN
            ALTER TABLE blocks ADD CONSTRAINT blocks_pkey PRIMARY KEY(id);
         END IF;
       END
       $$;`); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - blocks prevhash index...")
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS blocks_prevhash_idx ON blocks(prevhash);"); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - blocks hash index...")
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS blocks_hash_idx ON blocks(hash);"); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - blocks height index...")
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS blocks_height_idx ON blocks(height);"); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - txs primary key...")
	}
	if _, err := db.Exec(`
       DO $$
       BEGIN
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'txs' AND constraint_name = 'txs_pkey') THEN
            ALTER TABLE txs ADD CONSTRAINT txs_pkey PRIMARY KEY(id);
         END IF;
       END
       $$;`); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - txs txid (hash) index...")
	}
	if _, err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS txs_txid_idx ON txs(txid);"); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - block_txs block_id, n primary key...")
	}
	if _, err := db.Exec(`
       DO $$
       BEGIN
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'block_txs' AND constraint_name = 'block_txs_pkey') THEN
            ALTER TABLE block_txs ADD CONSTRAINT block_txs_pkey PRIMARY KEY(block_id, n);
         END IF;
       END
       $$;`); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - block_txs tx_id index...")
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS block_txs_tx_id_idx ON block_txs(tx_id);"); err != nil {
		return err
	}
	return nil
}

func createIndexes2(db *sql.DB, verbose bool) error {
	if verbose {
		log.Printf("  - utxos primary key...")
	}
	if _, err := db.Exec(`
	   DO $$
	   BEGIN
	     IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
	                     WHERE table_name = 'utxos' AND constraint_name = 'utxos_pkey') THEN
            ALTER TABLE utxos ALTER COLUMN tx_id SET NOT NULL;
	        ALTER TABLE utxos ADD CONSTRAINT utxos_pkey PRIMARY KEY(tx_id, n);
	     END IF;
	   END
	   $$;`); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - txins (prevout_tx_id, prevout_tx_n) index...")
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS txins_prevout_tx_id_prevout_n_idx ON txins(prevout_tx_id, prevout_n);"); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - txins primary key...")
	}
	if _, err := db.Exec(`
       DO $$
       BEGIN
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'txins' AND constraint_name = 'txins_pkey') THEN
            ALTER TABLE txins ADD CONSTRAINT txins_pkey PRIMARY KEY(tx_id, n);
         END IF;
       END
       $$;`); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - txouts primary key...")
	}
	if _, err := db.Exec(`
       DO $$
       BEGIN
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'txouts' AND constraint_name = 'txouts_pkey') THEN
            ALTER TABLE txouts ADD CONSTRAINT txouts_pkey PRIMARY KEY(tx_id, n);
         END IF;
       END
       $$;`); err != nil {
		return err
	}

	return nil
}

func createConstraints(db *sql.DB, verbose bool) error {
	if verbose {
		log.Printf("  - block_txs block_id foreign key...")
	}
	if _, err := db.Exec(`
	   DO $$
	   BEGIN
	     -- NB: table_name is the target/foreign table
	     IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
	                     WHERE table_name = 'blocks' AND constraint_name = 'block_txs_block_id_fkey') THEN
	       ALTER TABLE block_txs ADD CONSTRAINT block_txs_block_id_fkey FOREIGN KEY (block_id) REFERENCES blocks(id);
	     END IF;
	   END
	   $$;`); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - block_txs tx_id foreign key...")
	}
	if _, err := db.Exec(`
	   DO $$
	   BEGIN
	     -- NB: table_name is the target/foreign table
	     IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
	                     WHERE table_name = 'txs' AND constraint_name = 'block_txs_tx_id_fkey') THEN
	       ALTER TABLE block_txs ADD CONSTRAINT block_txs_tx_id_fkey FOREIGN KEY (tx_id) REFERENCES txs(id);
	     END IF;
	   END
	   $$;`); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - txins tx_id foreign key...")
	}
	if _, err := db.Exec(`
       DO $$
       BEGIN
         -- NB: table_name is the target/foreign table
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'txs' AND constraint_name = 'txins_tx_id_fkey') THEN
           ALTER TABLE txins ADD CONSTRAINT txins_tx_id_fkey FOREIGN KEY (tx_id) REFERENCES txs(id);
         END IF;
       END
       $$;`); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - txouts tx_id foreign key...")
	}
	if _, err := db.Exec(`
       DO $$
       BEGIN
         -- NB: table_name is the target/foreign table
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'txs' AND constraint_name = 'txouts_tx_id_fkey') THEN
           ALTER TABLE txouts ADD CONSTRAINT txouts_tx_id_fkey FOREIGN KEY (tx_id) REFERENCES txs(id);
         END IF;
       END
       $$;`); err != nil {
		return err
	}
	if verbose {
		log.Printf("  - utxos tx_id,n foreign key...")
	}
	if _, err := db.Exec(`
       DO $$
       BEGIN
         -- NB: table_name is the target/foreign table
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'txouts' AND constraint_name = 'utxos_tx_id_n_fkey') THEN
           ALTER TABLE utxos ADD CONSTRAINT utxos_tx_id_n_fkey FOREIGN KEY (tx_id, n) REFERENCES txouts(tx_id, n);
         END IF;
       END
       $$;`); err != nil {
		return err
	}
	return nil
}

// TODO: We already take care of this in leveldb.go?
//
// Set the orphan status starting from the highest block and going
// backwards, up to limit. If limit is 0, the whole table is updated.
//
// The WITH RECURSIVE part connects rows by joining prevhash to hash,
// thereby building a list which starts at the highest hight and going
// towards the beginning until no parent can be found.
//
// Then we LEFT JOIN the above to the blocks table, and where there is
// no match (x.id IS NULL) we mark it as orphan.
func setOrphans(db *sql.DB, limit int) error {
	var limitSql string
	if limit > 0 {
		limitSql = fmt.Sprintf("WHERE n < %d", limit)
	}
	if _, err := db.Exec(fmt.Sprintf(`
UPDATE blocks
   SET orphan = a.orphan
  FROM (
    SELECT blocks.id, x.id IS NULL AS orphan
      FROM blocks
      LEFT JOIN (
        WITH RECURSIVE recur(id, prevhash) AS (
          SELECT id, prevhash, 0 AS n
            FROM blocks
                            -- this should be faster than MAX(height)
           WHERE height IN (SELECT height FROM blocks ORDER BY height DESC LIMIT 1)
          UNION ALL
            SELECT blocks.id, blocks.prevhash, n+1 AS n
              FROM recur
              JOIN blocks ON blocks.hash = recur.prevhash
            %s
        )
        SELECT recur.id, recur.prevhash, n
          FROM recur
      ) x ON blocks.id = x.id
   ) a
  WHERE blocks.id = a.id;
       `, limitSql)); err != nil {
		return err
	}
	return nil
}

// Most of the prevout_tx_id's should be already set during the
// import, but we need to correct the remaining ones. This is a fairly
// costly operation as it requires a txins table scan.
func fixPrevoutTxId(db *sql.DB) error {
	if _, err := db.Exec(`
       DO $$
       BEGIN
         -- existence of txins_pkey means it is already done
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'txs' AND constraint_name = 'txins_tx_id_fkey') THEN
           UPDATE txins i
              SET prevout_tx_id = t.id
             FROM txs t
            WHERE i.prevout_hash = t.txid
              AND i.prevout_tx_id IS NULL
              AND i.n <> -1;

         END IF;
       END
       $$`); err != nil {
		return err
	}
	return nil
}

// // This populates spent column so that we can see that an output is
// // spent. The most efficient way of doing this insanely massive
// // operation is to create a new table, updating the existing one will
// // take an eternity.
// func markSpentOutputs(db *sql.DB) error {
// 	if _, err := db.Exec(`
//        DO $$
//        BEGIN
//          -- existence of txouts_pkey means it is already done
//          IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
//                          WHERE table_name = 'txouts' AND constraint_name = 'txouts_pkey') THEN
//            CREATE TABLE txouts_tmp AS
//              SELECT o.tx_id, o.n, o.value, o.scriptpubkey, i.prevout_tx_id IS NOT NULL AS spent
//                FROM txouts o
//                LEFT JOIN txins i
//                       ON i.prevout_tx_id = o.tx_id AND i.prevout_n = o.n;
//            DROP TABLE txouts;
//            ALTER TABLE txouts_tmp RENAME TO txouts;
//          END IF;
//        END
//        $$;`); err != nil {
// 		return err
// 	}
// 	return nil
// }

// Link UTXOs to transactions
func linkUTXOs(db *sql.DB) error {
	if _, err := db.Exec(`
       DO $$
       BEGIN
         -- existence of txouts_pkey means it is already done
         IF NOT EXISTS (SELECT constraint_name FROM information_schema.constraint_column_usage
                         WHERE table_name = 'utxos' AND constraint_name = 'utxos_pkey') THEN
           CREATE TABLE utxos_tmp AS
             SELECT t.id AS tx_id, u.txid, u.n, u.height, u.coinbase, u.value, u.scriptpubkey
               FROM utxos u
               JOIN txs t ON t.txid = u.txid;
           DROP TABLE utxos;
           ALTER TABLE utxos_tmp RENAME TO utxos;
         END IF;
       END
       $$;`); err != nil {
		return err
	}
	return nil
}
