package godiscoveryservice

import (
	"crypto"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"github.com/wayf-dk/gosaml"
	"github.com/wayf-dk/goxml"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

type (
	// Conf struct for reading the metadata feed
	Conf struct {
		DiscoMetaData string
		SpMetaData    string
	}

	idpInfoIn struct {
		EntityID     string        `json:"entityid"`
		DisplayNames []displayName `json:"DisplayNames"`
	}

	idpInfoOut struct {
		EntityID     string            `json:"entityID"`
		DisplayNames map[string]string `json:"DisplayNames"`
		Relevant     bool              `json:"relevant"`
	}

	spInfoOut struct {
		EntityID          string            `json:"entityID"`
		DisplayNames      map[string]string `json:"DisplayNames"`
		RequestInitiators []string          `json:"RequestInitiators"`
		Logo              string            `json:"Logo"`
	}

	displayName struct {
		Lang  string `json:"lang"`
		Value string `json:"value"`
	}

	response struct {
		Spok          bool         `json:"spok"`
		Chosen        []idpInfoOut `json:"chosen"`
		Found         int          `json:"found"`
		Rows          int          `json:"rows"`
		Feds          []string     `json:"feds"`
		Idps          []idpInfoOut `json:"idps"`
		Logo          string       `json:"logo"`
		Sp            spInfoOut    `json:"sp"`
		DiscoResponse []string     `json:"discoResponse"`
		DiscoACS      []string     `json:"discoACS"`
	}
)

var (
	// Config initialisation
	Config               = Conf{}
	dotdashpling         = regexp.MustCompile("[\\.\\-\\']")
	notword              = regexp.MustCompile("[^\\w]")
	whitespace           = regexp.MustCompile("[\\s]+|\\z")
	notwordnorwhitespace = regexp.MustCompile("[^\\s\\w]")
	spDB, idpDB          *sql.DB
	lock                 sync.Mutex
)

func MetadataUpdated() {
	lock.Lock()
	defer lock.Unlock()
	if spDB != nil {
		spDB.Close()
		spDB = nil
	}
	if idpDB != nil {
		idpDB.Close()
		idpDB = nil
	}
}

// DSTiming used for only logging response
func DSTiming(w http.ResponseWriter, r *http.Request) (err error) {
	w.Header().Set("Content-Type", "text/plain")
	return
}

// DSBackend takes the request extracts the entityID and returns an IDP
func DSBackend(w http.ResponseWriter, r *http.Request) (err error) {
	lock.Lock()
	defer lock.Unlock()

	var md []byte
	var spMetaData *goxml.Xp
	var res response
	r.ParseForm()
	entityID := r.Form.Get("entityID")
	ftsquery := strings.ToLower(string2Latin(r.Form.Get("query")))
	res.Feds = strings.Split(r.Form.Get("feds"), ",")
	res.Idps = []idpInfoOut{}
	chosen := strings.Split(r.Form.Get("chosen"), ",")
	providerIDs := strings.Split(r.Form.Get("providerids"), ",")

	if spDB == nil {
		spDB, err = sql.Open("sqlite3", Config.SpMetaData)
		if err != nil {
			return
		}
	}

	if idpDB == nil {
		idpDB, err = sql.Open("sqlite3", Config.DiscoMetaData)
		if err != nil {
			return
		}
	}

	providerIDsquery := ""
	if providerIDs[0] != "" {
		delim := "("
		for _, providerID := range providerIDs {
			providerID = notwordnorwhitespace.ReplaceAllLiteralString(providerID, "0")
			providerIDsquery += delim + "entityid:" + providerID
			delim = " OR "
		}
		providerIDsquery += ")"
	}

	if entityID != "" {
		//		defer db.Close()
		ent := hex.EncodeToString(goxml.Hash(crypto.SHA1, entityID))
		//		var query = "select e.md md from entity_HYBRID_INTERNAL e, lookup_HYBRID_INTERNAL l where l.hash = ? and l.entity_id_fk = e.id"
		var query = "select e.md md from entity_HYBRID_EXTERNAL_SP e, lookup_HYBRID_EXTERNAL_SP l where l.hash = ? and l.entity_id_fk = e.id"
		err = spDB.QueryRow(query, ent).Scan(&md)
		if err == nil {
			res.Sp.EntityID = entityID
			md = gosaml.Inflate([]byte(md))
			spMetaData = goxml.NewXp(md)
			res.Sp.Logo = spMetaData.Query1(nil, "md:SPSSODescriptor/md:Extensions/mdui:UIInfo/mdui:Logo")
			res.Sp.RequestInitiators = spMetaData.QueryMulti(nil, "md:SPSSODescriptor/md:Extensions/init:RequestInitiator/@Location")
			res.Sp.DisplayNames = map[string]string{}
			for _, l := range []string{"en", "da"} {
				res.Sp.DisplayNames[l] = spMetaData.Query1(nil, "md:SPSSODescriptor/md:Extensions/mdui:UIInfo/mdui:DisplayName[@xml:lang='"+l+"']")
			}
			if res.Feds[0] == "" {
				res.Feds = spMetaData.QueryMulti(nil, "md:Extensions/wayf:wayf/wayf:feds")
			}
			res.DiscoResponse = spMetaData.QueryMulti(nil, "md:SPSSODescriptor/md:Extensions/idpdisc:DiscoveryResponse[@Binding='urn:oasis:names:tc:SAML:profiles:SSO:idp-discovery-protocol']/@Location")
			res.DiscoACS = spMetaData.QueryMulti(nil, "md:SPSSODescriptor/md:AssertionConsumerService[@Binding='urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST']/@Location")
		}
	}

	res.Spok = (entityID == "") == (spMetaData == nil) // both either on or off

	if res.Spok {

		fedsquery := ""
		delim := "("
		for _, fed := range res.Feds {
			fed = notwordnorwhitespace.ReplaceAllLiteralString(fed, "0")
			fedsquery += delim + "feds:" + fed
			delim = " OR "
		}
		fedsquery += ")"

		if entityID != "" {
			chosenquery := ""
			if chosen[0] != "" {
				delim = "("
				for _, chosenentity := range chosen {
					chosenentity = notwordnorwhitespace.ReplaceAllLiteralString(chosenentity, "0")
					chosenquery += delim + chosenentity
					delim = " OR "
				}
				chosenquery += ")"
				//fmt.Fprintln(w, "chosenquery", chosenquery)

				// Find the still active earlier chosen IdPs - maybe the have gotten new displaynames
				rows, err := idpDB.Query("select json from disco where entityid MATCH ? limit 10", chosenquery)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var entityInfo []byte
					err = rows.Scan(&entityInfo)
					if err != nil {
						return err
					}

					var f idpInfoIn
					x := idpInfoOut{DisplayNames: map[string]string{}}
					err = json.Unmarshal(entityInfo, &f)
					if err != nil {
						return err
					}
					x.EntityID = f.EntityID
					//x.Keywords = keywords
					for _, dn := range f.DisplayNames {
						x.DisplayNames[dn.Lang] = dn.Value
					}

					res.Chosen = append(res.Chosen, x)
					//fmt.Fprintln(w, "chosen", res.Chosen)
				}
				// Find if earlier chosen IdPs are relevant
				rows, err = idpDB.Query("select json from disco where entityid MATCH ? limit 10", chosenquery+fedsquery+providerIDsquery)
				if err != nil {
					return err
				}

				defer rows.Close()
				for rows.Next() {
					var entityInfo []byte
					err = rows.Scan(&entityInfo)
					if err != nil {
						return err
					}
					var f idpInfoIn
					err = json.Unmarshal(entityInfo, &f)
					if err != nil {
						return err
					}
					// not that many - just iterate
					for i, chosen := range res.Chosen {
						if chosen.EntityID == f.EntityID {
							res.Chosen[i].Relevant = true
						}
					}
				}

				err = rows.Err()
				if err != nil {
					return err
				}
			}

		}

		ftsquery = whitespace.ReplaceAllLiteralString(ftsquery, "* ")

		// Find number of relevant IdPs
		err = idpDB.QueryRow("select count(*) c from disco where keywords MATCH ?", ftsquery+fedsquery+providerIDsquery).Scan(&res.Found)
		if err != nil {
			return err
		}
		// Find the first 100 relevant IdPs
		rows, err := idpDB.Query("select json from disco where keywords MATCH ? limit 100", ftsquery+fedsquery+providerIDsquery)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var entityInfo []byte
			err = rows.Scan(&entityInfo)
			if err != nil {
				return err
			}

			var f idpInfoIn
			x := idpInfoOut{DisplayNames: map[string]string{}}
			err = json.Unmarshal(entityInfo, &f)
			if err != nil {
				return err
			}
			x.EntityID = f.EntityID
			//x.Keywords = keywords
			for _, dn := range f.DisplayNames {
				x.DisplayNames[dn.Lang] = dn.Value
			}

			res.Idps = append(res.Idps, x)
			res.Rows++
			//fmt.Fprintln(w, "f", f)
		}
		err = rows.Err()
		if err != nil {
			return err
		}
	}
	b, err := json.Marshal(res)
	fmt.Fprintln(w, string(b))
	return
}
