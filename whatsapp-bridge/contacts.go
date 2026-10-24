package main

import (
	"context"
	"database/sql"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// contactDirectory is the part of the whatsmeow contact store used to name chats.
type contactDirectory interface {
	GetContact(ctx context.Context, user types.JID) (types.ContactInfo, error)
	GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error)
}

// lidDirectory maps between LIDs and phone-number JIDs.
type lidDirectory interface {
	GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error)
	GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error)
	GetManyLIDsForPNs(ctx context.Context, pns []types.JID) (map[types.JID]types.JID, error)
}

// contactResolver names individual chats from the user's own address book. WhatsApp
// now addresses most people by a LID (...@lid), while address-book entries are keyed
// by phone number, so a LID is first mapped to its phone number. Push names and
// business names are chosen by the other person and are never used.
type contactResolver struct {
	contacts contactDirectory
	lids     lidDirectory
}

func newContactResolver(client *whatsmeow.Client) contactResolver {
	return contactResolver{contacts: client.Store.Contacts, lids: client.Store.LIDs}
}

// phoneJID returns the phone-number JID for an individual JID, or an empty JID.
func (r contactResolver) phoneJID(ctx context.Context, jid types.JID) types.JID {
	jid = jid.ToNonAD()
	switch jid.Server {
	case types.DefaultUserServer:
		return jid
	case types.HiddenUserServer:
		if pn, err := r.lids.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return types.EmptyJID
}

// addressBookName returns the name the user saved for this person, or "".
func (r contactResolver) addressBookName(ctx context.Context, jid types.JID) string {
	jid = jid.ToNonAD()
	for _, candidate := range []types.JID{jid, r.phoneJID(ctx, jid)} {
		if candidate.IsEmpty() {
			continue
		}
		if info, err := r.contacts.GetContact(ctx, candidate); err == nil && info.FullName != "" {
			return info.FullName
		}
	}
	return ""
}

// name returns the address-book name, else the phone number, else the JID's user part.
func (r contactResolver) name(ctx context.Context, jid types.JID) string {
	if name := r.addressBookName(ctx, jid); name != "" {
		return name
	}
	if pn := r.phoneJID(ctx, jid); !pn.IsEmpty() {
		return pn.User
	}
	return jid.ToNonAD().User
}

func isIndividualServer(server string) bool {
	return server == types.DefaultUserServer || server == types.HiddenUserServer
}

// SyncContacts mirrors the address book and the LID-to-phone mappings into the
// contacts table, which the MCP server uses to name group senders and to match phone
// numbers to LID chats. It also renames individual chats from the same source, which
// fixes chats stored with a placeholder name. It returns the number of renamed chats.
func (store *MessageStore) SyncContacts(ctx context.Context, r contactResolver) (int, error) {
	chatNames := map[types.JID]string{}
	rows, err := store.db.QueryContext(ctx, "SELECT jid, name FROM chats WHERE jid LIKE '%@lid' OR jid LIKE '%@s.whatsapp.net'")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var raw string
		var name sql.NullString
		if err := rows.Scan(&raw, &name); err != nil {
			rows.Close()
			return 0, err
		}
		if jid, err := types.ParseJID(raw); err == nil && isIndividualServer(jid.Server) {
			chatNames[jid] = name.String
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	known := map[types.JID]bool{}
	for jid := range chatNames {
		known[jid] = true
	}
	contacts, err := r.contacts.GetAllContacts(ctx)
	if err != nil {
		return 0, err
	}
	var pns []types.JID
	for jid := range contacts {
		jid = jid.ToNonAD()
		if isIndividualServer(jid.Server) {
			known[jid] = true
		}
		if jid.Server == types.DefaultUserServer {
			pns = append(pns, jid)
		}
	}
	if len(pns) > 0 {
		lids, err := r.lids.GetManyLIDsForPNs(ctx, pns)
		if err != nil {
			return 0, err
		}
		for _, lid := range lids {
			if !lid.IsEmpty() {
				known[lid.ToNonAD()] = true
			}
		}
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM contacts"); err != nil {
		return 0, err
	}
	insert, err := tx.PrepareContext(ctx, "INSERT INTO contacts (jid, phone, name) VALUES (?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer insert.Close()
	for jid := range known {
		phone, name := r.phoneJID(ctx, jid).User, r.addressBookName(ctx, jid)
		if phone == "" && name == "" {
			continue
		}
		if _, err := insert.ExecContext(ctx, jid.String(), nullIfEmpty(phone), nullIfEmpty(name)); err != nil {
			return 0, err
		}
	}

	renamed := 0
	for jid, old := range chatNames {
		if name := r.name(ctx, jid); name != "" && name != old {
			if _, err := tx.ExecContext(ctx, "UPDATE chats SET name = ? WHERE jid = ?", name, jid.String()); err != nil {
				return 0, err
			}
			renamed++
		}
	}
	return renamed, tx.Commit()
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
