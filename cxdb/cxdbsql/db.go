package cxdbsql

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"sync"

	"github.com/mit-dci/lit/coinparam"

	// mysql is just the driver, always interact with database/sql api
	_ "github.com/go-sql-driver/mysql"
	"github.com/mit-dci/opencx/logging"
	"github.com/mit-dci/opencx/match"
)

// turn into config options
var (
	// definitely move this to a config file
	balanceSchema        = "balances"
	depositSchema        = "deposit"
	pendingDepositSchema = "pending_deposits"
	orderSchema          = "orders"
	peerSchema           = "peers"
	peerTableName        = "opencxpeers"
)

// DB contains the sql DB type as well as a logger.
// The database is a BEHEMOTH, should be refactored. Some examples on how to refactor are cleaning up mutexes, creating config file for all the globals,
// What would be great is to move everything having to do with price and matching into match and making match more like a matching engine framework
// or library for exchanges. This should conform to the cxdb interface, and if the server uses the noise protocol / authenticated networking, or anything
// that requires conforming to the lncore.LitPeerStorage interface, it should conform to that as well.
type DB struct {
	// the SQL handler for the db
	DBHandler *sql.DB

	// db username and password
	dbUsername string
	dbPassword string
	// db host and port
	dbAddr net.Addr

	// standard exchange stuff
	// name of balance schema
	balanceSchema string
	// name of deposit schema
	depositSchema string
	// name of pending deposit schema
	pendingDepositSchema string
	// name of order schema
	orderSchema string

	// peer schema stuff
	// name of peer schema
	peerSchema string
	// name of peer table
	peerTableName string

	// auction schema stuff
	// name of puzzle orderbook schema
	puzzleSchema string
	// name of the auction orderbook schema
	auctionSchema string
	// name of the auction ID => auction number map schema
	auctionOrderSchema string
	// name of the auction ID => auction number table
	auctionOrderTable string

	// list of all coins supported, passed in from above
	coinList []*coinparam.Params

	// the pairs that are supported. generated from coinList when the db is initiated
	pairsArray []*match.Pair

	// pricemap for pair that we manually add to
	gPriceMap map[string]float64
	// priceMapMtx is a lock for gPriceMap
	priceMapMtx *sync.Mutex
}

// SetPrice sets the price, uses a lock since it will be written to and read from possibly at the same time (written to by server, read by client)
func (db *DB) SetPrice(newPrice float64, pairString string) {
	db.priceMapMtx.Lock()
	db.gPriceMap[pairString] = newPrice
	db.priceMapMtx.Unlock()
}

// GetPrice returns the price and side of the last transacted price
func (db *DB) GetPrice(pairString string) (price float64, err error) {
	var found bool
	if price, found = db.gPriceMap[pairString]; !found {
		err = fmt.Errorf("Could not get price, pair not found")
	}
	return
}

// GetPairs returns the pairs list
func (db *DB) GetPairs() (pairArray []*match.Pair) {
	pairArray = db.pairsArray
	return
}

// CreateDBConnection initializes the db so the client is ready to be set up. We use TCP by default
func CreateDBConnection(username string, password string, host string, port uint16) (dbconn *DB, err error) {
	var dbAddr net.Addr
	if dbAddr, err = net.ResolveTCPAddr("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port))); err != nil {
		err = fmt.Errorf("Error resolving database address: \n%s", err)
		return
	}

	dbconn = &DB{
		dbAddr:     dbAddr,
		dbUsername: username,
		dbPassword: password,
	}
	return
}

// SetupClient sets up the mysql client and driver
func (db *DB) SetupClient(coinList []*coinparam.Params) (err error) {
	db.gPriceMap = make(map[string]float64)
	db.balanceSchema = balanceSchema
	db.depositSchema = depositSchema
	db.pendingDepositSchema = pendingDepositSchema
	db.orderSchema = orderSchema
	db.peerSchema = peerSchema
	db.peerTableName = peerTableName
	// Create users and schemas and assign permissions to opencx
	if err = db.rootInitSchemas(); err != nil {
		err = fmt.Errorf("Root could not initialize schemas: \n%s", err)
		return
	}

	// open db handle
	openString := fmt.Sprintf("%s:%s@%s(%s)/", db.dbUsername, db.dbPassword, db.dbAddr.Network(), db.dbAddr.String())

	var dbHandle *sql.DB
	if dbHandle, err = sql.Open("mysql", openString); err != nil {
		err = fmt.Errorf("Error opening database: \n%s", err)
		return
	}

	db.DBHandler = dbHandle
	db.coinList = coinList

	// Get all the pairs
	if db.pairsArray, err = match.GenerateAssetPairs(coinList); err != nil {
		return
	}

	// DEBUGGING
	// Get all the assets
	for i, asset := range db.coinList {
		logging.Debugf("Asset %d: %s\n", i, asset.Name)
	}

	// Get all the asset pairs
	for i, pair := range db.pairsArray {
		logging.Debugf("Pair %d: %s\n", i, pair)
	}
	// END DEBUGGING

	if err = db.DBHandler.Ping(); err != nil {
		err = fmt.Errorf("Could not ping the database, is it running: %s", err)
		return
	}

	if err = db.SetupCustodyTables(db.balanceSchema, db.depositSchema, db.pendingDepositSchema); err != nil {
		err = fmt.Errorf("Error setting up custody tables: %s", err)
		return
	}

	if err = db.SetupExchangeTables(db.orderSchema); err != nil {
		err = fmt.Errorf("Error setting up exchange tables: %s", err)
		return
	}

	return
}

// SetupPeerTables sets up tables required for the database to conform to Lit Peer Storage
func (db *DB) SetupPeerTables(peerSchema string, peerTable string) (err error) {

	// peer schema stuff
	// Initialize peer table
	// TODO: change this when peeridx is deprecated, if it ever is
	if err = db.InitializeSingleTable(db.peerSchema, db.peerTableName, "lnaddr VARBINARY(40), name TEXT, netaddr TEXT, peerIdx INT(32) UNSIGNED"); err != nil {
		err = fmt.Errorf("Could not initialize peer tables: \n%s", err)
		return
	}

	return
}

// SetupCustodyTables sets up the tables needed to track what funds a user has
func (db *DB) SetupCustodyTables(balanceSchema string, depositSchema string, pendingDepositSchema string) (err error) {

	// Initialize Balance tables
	// 66 bytes because we use big bytes and they use small bytes for varbinary
	if err = db.InitializeTables(balanceSchema, "pubkey VARBINARY(66), balance BIGINT(64)"); err != nil {
		err = fmt.Errorf("Could not initialize balance tables: \n%s", err)
		return
	}

	// Initialize Deposit tables
	if err = db.InitializeTables(depositSchema, "pubkey VARBINARY(66), address VARCHAR(34), CONSTRAINT unique_pubkeys UNIQUE (pubkey, address)"); err != nil {
		err = fmt.Errorf("Could not initialize deposit tables: \n%s", err)
		return
	}

	// Initialize pending_deposits table
	if err = db.InitializeNewTables(pendingDepositSchema, "pubkey VARBINARY(66), expectedConfirmHeight INT(32) UNSIGNED, depositHeight INT(32) UNSIGNED, amount BIGINT(64), txid TEXT"); err != nil {
		err = fmt.Errorf("Could not initialize pending deposit tables: \n%s", err)
		return
	}

	return
}

// SetupExchangeTables sets up the tables needed for an orderbook
func (db *DB) SetupExchangeTables(orderSchema string) (err error) {
	// Initialize order table
	// You can have a price up to 30 digits total, and 10 decimal places.
	if err = db.InitializePairTables(db.orderSchema, "pubkey VARBINARY(66), orderID TEXT, side TEXT, price DOUBLE(30,2) UNSIGNED, amountHave BIGINT(64), amountWant BIGINT(64), time TIMESTAMP"); err != nil {
		err = fmt.Errorf("Could not initialize order tables: \n%s", err)
		return
	}
	return
}

// SetupAuctionTables sets up the tables needed to store auction orders and puzzles for specific auctions
func (db *DB) SetupAuctionTables(initialAuctionID [32]byte) (err error) {

	// Initialize auction order schema, table
	// An auction order is identified by it's auction ID, pubkey, nonce, and other specific data.
	// You can have a price up to 30 digits total, and 10 decimal places.
	if err = db.InitializePairTables(db.auctionSchema, "pubkey VARBINARY(66), orderID TEXT, side TEXT, price DOUBLE(30,2) UNSIGNED, amountHave BIGINT(64), amountWant BIGINT(64), auctionID VARBINARY(64), nonce VARBINARY(4), hashedOrder VARBINARY"); err != nil {
		err = fmt.Errorf("Could not initialize order tables: \n%s", err)
		return
	}

	// Yes yes I know fmt.Sprintf("%x", initialAuctionID) exists, this seems to get the point across quicker
	initialAuctionTableName := hex.EncodeToString(initialAuctionID[:])
	// the name of the table is the auction ID: We can just get auction puzzles by table. "selected" is whether or not it's been selected for the auction.
	if err = db.InitializeSingleTable(db.puzzleSchema, initialAuctionTableName, "encodedPuzzle VARBINARY, selected BOOLEAN"); err != nil {
		err = fmt.Errorf("Could not initialize puzzle tables: \n%s", err)
		return
	}

	return
}

// InitializeSingleTable initializes a single table in a schema
func (db *DB) InitializeSingleTable(schemaName string, tableName string, schemaSpec string) (err error) {

	// Use the schema
	if _, err = db.DBHandler.Exec("USE " + schemaName + ";"); err != nil {
		err = fmt.Errorf("Could not use %s schema: \n%s", schemaName, err)
		return
	}
	tableQuery := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s);", tableName, schemaSpec)
	if _, err = db.DBHandler.Exec(tableQuery); err != nil {
		err = fmt.Errorf("Could not create table %s: \n%s", tableName, err)
		return
	}
	return
}

// InitializeTables initializes all of the tables necessary for the exchange to run, using the coin list that is set up.
func (db *DB) InitializeTables(schemaName string, schemaSpec string) (err error) {

	// Use the schema
	if _, err = db.DBHandler.Exec("USE " + schemaName + ";"); err != nil {
		err = fmt.Errorf("Could not use %s schema: \n%s", schemaName, err)
		return
	}
	for _, chain := range db.coinList {
		tableQuery := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s);", chain.Name, schemaSpec)
		if _, err = db.DBHandler.Exec(tableQuery); err != nil {
			err = fmt.Errorf("Could not create table %s: \n%s", chain.Name, err)
			return
		}
	}
	return
}

// InitializeNewTables initalizes tables based on schema and clears them.
func (db *DB) InitializeNewTables(schemaName string, schemaSpec string) (err error) {
	// Use the schema
	if _, err = db.DBHandler.Exec("USE " + schemaName + ";"); err != nil {
		err = fmt.Errorf("Could not use %s schema: \n%s", schemaName, err)
		return
	}
	for _, chain := range db.coinList {
		tableQuery := fmt.Sprintf("CREATE OR REPLACE TABLE %s (%s);", chain.Name, schemaSpec)
		if _, err = db.DBHandler.Exec(tableQuery); err != nil {
			err = fmt.Errorf("Could not create table %s: \n%s", chain.Name, err)
			return
		}
		deleteQuery := fmt.Sprintf("DELETE FROM %s;", chain.Name)
		if _, err = db.DBHandler.Exec(deleteQuery); err != nil {
			err = fmt.Errorf("Could not delete stuff from table after creating: \n%s", err)
			return
		}
	}
	return
}

// InitializePairTables initializes tables per pair
func (db *DB) InitializePairTables(schemaName string, schemaSpec string) (err error) {
	// Use the schema
	if _, err = db.DBHandler.Exec("USE " + schemaName + ";"); err != nil {
		err = fmt.Errorf("Could not use %s schema: \n%s", schemaName, err)
		return
	}
	for _, pair := range db.pairsArray {
		tableQuery := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s);", pair.String(), schemaSpec)
		if _, err = db.DBHandler.Exec(tableQuery); err != nil {
			err = fmt.Errorf("Could not create table %s: \n%s", pair.String(), err)
			return
		}
	}
	return
}

// rootInitSchemas initalizes the schemas, creates users, and grants permissions to those users
func (db *DB) rootInitSchemas() (err error) {

	// open db handle
	openString := fmt.Sprintf("%s:%s@%s(%s)/", db.dbUsername, db.dbPassword, db.dbAddr.Network(), db.dbAddr.String())
	var rootHandler *sql.DB
	if rootHandler, err = sql.Open("mysql", openString); err != nil {
		err = fmt.Errorf("Error opening database: \n%s", err)
		return
	}

	// When the method is done, close the root connection
	defer rootHandler.Close()

	if err = rootHandler.Ping(); err != nil {
		err = fmt.Errorf("Could not ping the database, is it running: \n%s", err)
		return
	}

	schemasToCreate := []string{
		db.balanceSchema,
		db.depositSchema,
		db.pendingDepositSchema,
		db.orderSchema,
		db.peerSchema,
	}

	for _, schema := range schemasToCreate {
		if err = rootCreateSchemaForUser(rootHandler, db.dbUsername, schema); err != nil {
			err = fmt.Errorf("Error calling rootCreateSchemaForUser helper: \n%s", err)
			return
		}

	}

	return
}

// Helper function for db
func rootCreateSchemaForUser(rootHandler *sql.DB, username string, schemaString string) (err error) {
	// check pending deposit schema
	// if pending deposit schema not there make it
	if _, err = rootHandler.Exec("CREATE SCHEMA IF NOT EXISTS " + schemaString + ";"); err != nil {
		err = fmt.Errorf("Could not create %s schema: \n%s", schemaString, err)
		return
	}

	return
}
