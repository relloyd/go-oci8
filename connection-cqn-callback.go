package oci8

// #include "oci8.go.h"
// #include <string.h>
// #include <stdio.h>
// #include <stdlib.h>
// sword getCollectionElement(OCIEnv* envhp, OCIError* errhp, OCIColl* collection, ub2 idx, dvoid** element);
import "C"

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"unsafe"
)

//export goCqnCallback
func goCqnCallback(ctx unsafe.Pointer, subHandle *C.OCISubscription, payload unsafe.Pointer, payl *C.ub4, descriptor unsafe.Pointer, mode C.ub4) {
	var err error
	var result C.sword
	conn := OCI8Conn{}
	if conn.logger == nil {
		conn.logger = log.New(ioutil.Discard, "", 0)
	}
	// Defer cleanup.
	defer conn.freeHandles()
	// Environment handle.
	var envP *C.OCIEnv
	envPP := &envP
	charset := C.ub2(0)
	if os.Getenv("NLS_LANG") == "" && os.Getenv("NLS_NCHAR") == "" {
		charset = defaultCharset
	}
	envCreateFlags := C.OCI_EVENTS | C.OCI_OBJECT // required for ContinuousQueryNotification.
	result = C.OCIEnvNlsCreate(
		envPP,                 // pointer to a handle to the environment
		C.ub4(envCreateFlags), // environment mode: https://docs.oracle.com/cd/B28359_01/appdev.111/b28395/oci16rel001.htm#LNOCI87683
		nil,                   // Specifies the user-defined context for the memory callback routines.
		nil,                   // Specifies the user-defined memory allocation function. If mode is OCI_THREADED, this memory allocation routine must be thread-safe.
		nil,                   // Specifies the user-defined memory re-allocation function. If the mode is OCI_THREADED, this memory allocation routine must be thread safe.
		nil,                   // Specifies the user-defined memory free function. If mode is OCI_THREADED, this memory free routine must be thread-safe.
		0,                     // Specifies the amount of user memory to be allocated for the duration of the environment.
		nil,                   // Returns a pointer to the user memory of size xtramemsz allocated by the call for the user.
		charset,               // The client-side character set for the current environment handle. If it is 0, the NLS_LANG setting is used.
		charset,               // The client-side national character set for the current environment handle. If it is 0, NLS_NCHAR setting is used.
	)
	if result != C.OCI_SUCCESS {
		panic("OCIEnvNlsCreate error")
	}
	conn.env = *envPP
	// Error handle.
	var handleTemp unsafe.Pointer
	handle := &handleTemp
	result = C.OCIHandleAlloc(
		unsafe.Pointer(conn.env), // An environment handle
		handle,                   // Returns a handle
		C.OCI_HTYPE_ERROR,        // type of handle: https://docs.oracle.com/cd/B28359_01/appdev.111/b28395/oci02bas.htm#LNOCI87581
		0,                        // amount of user memory to be allocated
		nil,                      // Returns a pointer to the user memory
	)
	if result != C.OCI_SUCCESS {
		panic("error allocating Oracle error handle")
	}
	conn.errHandle = (*C.OCIError)(*handle)
	handle = nil // deallocate.
	// Get the notification type from the descriptor.
	var notificationType C.ub4
	result = C.OCIAttrGet(descriptor, C.OCI_DTYPE_CHDES, unsafe.Pointer(&notificationType), nil, C.OCI_ATTR_CHDES_NFYTYPE, conn.errHandle)
	if err = conn.getError(result); err != nil {
		panic("error fetching CQN notification type")
	}
	fmt.Println("notification type =", notificationType)
	// Process changes based on notification type.
	var tableChangesPtr *C.OCIColl
	var queryChangesPtr *C.OCIColl
	if notificationType == C.OCI_EVENT_SHUTDOWN || notificationType == C.OCI_EVENT_SHUTDOWN_ANY { // if the database is shutting down...
		fmt.Println("Oracle shutdown notification received")
		return
	} else if notificationType == C.OCI_EVENT_STARTUP { // if the database is starting up...
		fmt.Println("Oracle startup notification received")
		return
	} else if notificationType == C.OCI_EVENT_OBJCHANGE { // else if we registered a subscription of type OCI_SUBSCR_CQ_QOS_BEST_EFFORT...
		// Supply address of pointer tableChangesPtr *C.OCIColl to OCIAttrGet.
		// This isn't exactly clear from the documentation:
		// void* is the documented type, but (void*)(&*C.OCIColl) seems to work!
		result = C.OCIAttrGet(descriptor, C.OCI_DTYPE_CHDES, unsafe.Pointer(&tableChangesPtr), nil, C.OCI_ATTR_CHDES_TABLE_CHANGES, conn.errHandle)
		if err = conn.getError(result); err != nil {
			panic("error fetching CQN table changes")
		}
		fmt.Println("processing table changes...")
		extractTableChanges(&conn, tableChangesPtr)
	} else if notificationType == C.OCI_EVENT_QUERYCHANGE { // else if we registered subscription of type OCI_SUBSCR_CQ_QOS_QUERY...
		result = C.OCIAttrGet(descriptor, C.OCI_DTYPE_CHDES, unsafe.Pointer(&queryChangesPtr), nil, C.OCI_ATTR_CHDES_QUERIES, conn.errHandle)
		if err = conn.getError(result); err != nil {
			panic("error fetching CQN query changes")
		}
		fmt.Println("processing query changes")
		// TODO: process query changes!
		// processQueryChanges(envhp, errhp, stmthp, queryChanges)
	}
	// Get the registration ID.
	// Alternatively use C.OCI_ATTR_CQ_QUERYID to get the query ID. This produces return value = 0, for what I think
	// is the first query since multiples can be registered in one subscription.
	// The attribute type: https://docs.oracle.com/cd/B19306_01/appdev.102/b14250/ociaahan.htm
	var regId C.ub8
	regIdSize := C.ub4(C.sizeof_ub8)
	result = C.OCIAttrGet(unsafe.Pointer(subHandle), C.OCI_HTYPE_SUBSCRIPTION, unsafe.Pointer(&regId), &regIdSize, C.OCI_ATTR_SUBSCR_CQ_REGID, conn.errHandle)
	if err = conn.getError(result); err != nil {
		panic("error fetching CQN registration ID in callback")
	}
	log.Println("callback fetched registration ID =", int64(regId))
}

// extractTableChanges will extract the table changes.
// It expects conn.env and conn.errHandle to be setup in advance.
func extractTableChanges(conn *OCI8Conn, tableChanges *C.OCIColl) {
	var err error
	var result C.sword
	var element unsafe.Pointer // will be populated by call to getCollectionElement().
	var tableNameOratext *C.oratext
	var tableOp C.ub4
	var rowChanges *C.OCIColl
	// Get the number of table changes.
	numTables := getCollSize(conn, tableChanges)
	fmt.Println("number of table changes is", numTables)
	// Process each table in the change list.
	for idx := 0; idx < numTables; idx++ { // for each table in the collection...
		// Get the collection element and fetch the attributes within it.
		result = C.getCollectionElement(conn.env, conn.errHandle, tableChanges, C.ub2(idx), &element)
		if err = conn.getError(result); err != nil {
			panic(fmt.Sprintf("error fetching table changes element: %v", err))
		}
		// Extract the table name.
		result = C.OCIAttrGet(element, C.OCI_DTYPE_TABLE_CHDES, unsafe.Pointer(&tableNameOratext), nil, C.OCI_ATTR_CHDES_TABLE_NAME, conn.errHandle)
		if err = conn.getError(result); err != nil {
			panic("error fetching table name from element")
		}
		// fmt.Println("table name =", oraText2GoString(tableNameOratext))
		// Extract the operation type.
		result = C.OCIAttrGet(element, C.OCI_DTYPE_TABLE_CHDES, unsafe.Pointer(&tableOp), nil, C.OCI_ATTR_CHDES_TABLE_OPFLAGS, conn.errHandle)
		if err = conn.getError(result); err != nil {
			panic("error fetching table operation from element")
		}
		// Find out if there were row changes.
		result = C.OCIAttrGet(element, C.OCI_DTYPE_TABLE_CHDES, unsafe.Pointer(&rowChanges), nil, C.OCI_ATTR_CHDES_TABLE_ROW_CHANGES, conn.errHandle)
		if err = conn.getError(result); err != nil {
			panic("error fetching row changes")
		}
		// Dump table changes.
		fmt.Println(fmt.Sprintf("Table changed is %v; tableOp = 0x%x", oraText2GoString(tableNameOratext), int32(tableOp)))
		// Process row changes.
		if !((tableOp & C.ub4(C.OCI_OPCODE_ALLROWS)) > 0) { // if individual rows were changed...
			// processRowChanges(envhp, errhp, stmthp, row_changes);
			fmt.Println("processing row changes...")
			extractRowChanges(conn, rowChanges)
		} else {
			fmt.Println("all rows changed")
		}
	}
}

func extractRowChanges(conn *OCI8Conn, rowChanges *C.OCIColl) {
	var err error
	var result C.sword
	var element unsafe.Pointer
	var rowIdOratext *C.oratext
	var rowOp C.ub4
	// Get the number of row changes.
	numChanges := getCollSize(conn, rowChanges)
	fmt.Println("number of row changes =", numChanges)
	// Process each row in the change list.
	for idx := 0; idx < numChanges; idx++ { // for each row change...
		// Extract the element and fetch attributes within it.
		result = C.getCollectionElement(conn.env, conn.errHandle, rowChanges, C.ub2(idx), &element)
		if err = conn.getError(result); err != nil {
			panic("error fetching collection element from row changes")
		}
		result = C.OCIAttrGet(element, C.OCI_DTYPE_ROW_CHDES, unsafe.Pointer(&rowIdOratext), nil, C.OCI_ATTR_CHDES_ROW_ROWID, conn.errHandle)
		if err = conn.getError(result); err != nil {
			panic("error fetching row ID")
		}
		result = C.OCIAttrGet(element, C.OCI_DTYPE_ROW_CHDES, unsafe.Pointer(&rowOp), nil, C.OCI_ATTR_CHDES_ROW_OPFLAGS, conn.errHandle)
		if err = conn.getError(result); err != nil {
			panic("error fetching row operation")
		}
		fmt.Println(fmt.Sprintf("Row changed = %v; rowOp = 0x%x", oraText2GoString(rowIdOratext), int32(rowOp)))
	}
}

// oraText2GoString coverts C oratext to Go string.
func oraText2GoString(s *C.oratext) string {
	p := (*[1 << 30]byte)(unsafe.Pointer(s))
	size := 0
	for p[size] != 0 { // while we look for a null string terminator...
		size++
	}
	buf := make([]byte, size)
	copy(buf, p[:])
	return *(*string)(unsafe.Pointer(&buf))
}

// getCollSize returns the number of elements in the collection.
func getCollSize(conn *OCI8Conn, c *C.OCIColl) int {
	var err error
	var result C.sword
	var size C.sb4
	result = C.OCICollSize(conn.env, conn.errHandle, c, &size)
	err = conn.getError(result)
	if err != nil {
		panic("error getting CQN collection size")
	}
	return int(size)
}
