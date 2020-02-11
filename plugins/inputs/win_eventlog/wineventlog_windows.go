// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// +build windows

package win_eventlog

import (
	"errors"
	"fmt"
	"io"
	"syscall"

	"golang.org/x/sys/windows"
)

// Errors
var (
	// ErrorEvtVarTypeNull is an error that means the content of the EVT_VARIANT
	// data is null.
	ErrorEvtVarTypeNull = errors.New("null EVT_VARIANT data")
)

// bookmarkTemplate is a parameterized string that requires two parameters,
// the channel name and the record ID. The formatted string can be used to open
// a new event log subscription and resume from the given record ID.
const bookmarkTemplate = `<BookmarkList><Bookmark Channel="%s" RecordId="%d" ` +
	`IsCurrent="True"/></BookmarkList>`

// InsufficientBufferError indicates the buffer passed to a system call is too
// small.
type InsufficientBufferError struct {
	Cause        error
	RequiredSize int // Size of the buffer that is required.
}

// Error returns the cause of the insufficient buffer error.
func (e InsufficientBufferError) Error() string {
	return e.Cause.Error()
}

// IsAvailable returns true if the Windows Event Log API is supported by this
// operating system. If not supported then false is returned with the
// accompanying error.
func IsAvailable() (bool, error) {
	err := modwevtapi.Load()
	if err != nil {
		return false, err
	}

	return true, nil
}

// EvtQuery runs a query to retrieve events from a channel or log file that
// match the specified query criteria.
func EvtQuery(session EvtHandle, path string, query string, flags EvtQueryFlag) (EvtHandle, error) {
	var err error
	var pathPtr *uint16
	if path != "" {
		pathPtr, err = syscall.UTF16PtrFromString(path)
		if err != nil {
			return 0, err
		}
	}

	var queryPtr *uint16
	if query != "" {
		queryPtr, err = syscall.UTF16PtrFromString(query)
		if err != nil {
			return 0, err
		}
	}

	return _EvtQuery(session, pathPtr, queryPtr, uint32(flags))
}

// Subscribe creates a new subscription to an event log channel.
func Subscribe(
	session EvtHandle,
	event windows.Handle,
	channelPath string,
	query string,
	bookmark EvtHandle,
	flags EvtSubscribeFlag,
) (EvtHandle, error) {
	var err error
	var cp *uint16
	if channelPath != "" {
		cp, err = syscall.UTF16PtrFromString(channelPath)
		if err != nil {
			return 0, err
		}
	}

	var q *uint16
	if query != "" {
		q, err = syscall.UTF16PtrFromString(query)
		if err != nil {
			return 0, err
		}
	}

	eventHandle, err := _EvtSubscribe(session, uintptr(event), cp, q, bookmark,
		0, 0, flags)
	if err != nil {
		return 0, err
	}

	return eventHandle, nil
}

// EventHandles reads the event handles from a subscription. It attempt to read
// at most maxHandles. ErrorNoMoreHandles is returned when there are no more
// handles available to return. Close must be called on each returned EvtHandle
// when finished with the handle.
func EventHandles(subscription EvtHandle, maxHandles int) ([]EvtHandle, error) {
	if maxHandles < 1 {
		return nil, fmt.Errorf("maxHandles must be greater than 0")
	}

	eventHandles := make([]EvtHandle, maxHandles)
	var numRead uint32

	err := _EvtNext(subscription, uint32(len(eventHandles)),
		&eventHandles[0], 0, 0, &numRead)
	if err != nil {
		// Munge ERROR_INVALID_OPERATION to ERROR_NO_MORE_ITEMS when no handles
		// were read. This happens you call the method and there are no events
		// to read (i.e. polling).
		if err == ERROR_INVALID_OPERATION && numRead == 0 {
			return nil, ERROR_NO_MORE_ITEMS
		}
		return nil, err
	}

	return eventHandles[:numRead], nil
}

// RenderEventXML renders the event as XML. If the event is already rendered, as
// in a forwarded event whose content type is "RenderedText", then the XML will
// include the RenderingInfo (message). If the event is not rendered then the
// XML will not include the message, and in this case RenderEvent should be
// used.
func RenderEventXML(eventHandle EvtHandle, renderBuf []byte, out io.Writer) error {
	return renderXML(eventHandle, EvtRenderEventXml, renderBuf, out)
}

// RenderBookmarkXML renders a bookmark as XML.
func RenderBookmarkXML(bookmarkHandle EvtHandle, renderBuf []byte, out io.Writer) error {
	return renderXML(bookmarkHandle, EvtRenderBookmark, renderBuf, out)
}

// CreateBookmarkFromEvent creates a new bookmark pointing to the given event.
// Close must be called on returned EvtHandle when finished with the handle.
func CreateBookmarkFromEvent(handle EvtHandle) (EvtHandle, error) {
	h, err := _EvtCreateBookmark(nil)
	if err != nil {
		return 0, err
	}
	if err = _EvtUpdateBookmark(h, handle); err != nil {
		return 0, err
	}
	return h, nil
}

// Close closes an EvtHandle.
func Close(h EvtHandle) error {
	return _EvtClose(h)
}

func renderXML(eventHandle EvtHandle, flag EvtRenderFlag, renderBuf []byte, out io.Writer) error {
	var bufferUsed, propertyCount uint32
	err := _EvtRender(0, eventHandle, flag, uint32(len(renderBuf)),
		&renderBuf[0], &bufferUsed, &propertyCount)
	if err == ERROR_INSUFFICIENT_BUFFER {
		return InsufficientBufferError{Cause: err, RequiredSize: int(bufferUsed)}
	}
	if err != nil {
		return err
	}

	if int(bufferUsed) > len(renderBuf) {
		return fmt.Errorf("Windows EvtRender reported that wrote %d bytes "+
			"to the buffer, but the buffer can only hold %d bytes",
			bufferUsed, len(renderBuf))
	}
	return UTF16ToUTF8Bytes(renderBuf[:bufferUsed], out)
}
