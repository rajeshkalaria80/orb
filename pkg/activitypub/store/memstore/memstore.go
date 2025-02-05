/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package memstore

import (
	"fmt"
	"net/url"
	"sort"
	"sync"

	"github.com/trustbloc/orb/internal/pkg/log"
	"github.com/trustbloc/orb/pkg/activitypub/store/spi"
	"github.com/trustbloc/orb/pkg/activitypub/store/storeutil"
	"github.com/trustbloc/orb/pkg/activitypub/vocab"
)

const loggerModule = "activitypub_memstore"

// Store implements an in-memory ActivityPub store.
type Store struct {
	activityStore   *activityStore
	referenceStores map[spi.ReferenceType]*referenceStore
	logger          *log.StructuredLog
}

// New returns a new in-memory ActivityPub store.
func New(serviceName string) *Store {
	return &Store{
		activityStore: newActivitiesStore(),
		logger:        log.NewStructured(loggerModule, log.WithFields(log.WithServiceName(serviceName))),
		referenceStores: map[spi.ReferenceType]*referenceStore{
			spi.Inbox:         newReferenceStore(),
			spi.Outbox:        newReferenceStore(),
			spi.PublicOutbox:  newReferenceStore(),
			spi.Follower:      newReferenceStore(),
			spi.Following:     newReferenceStore(),
			spi.Witness:       newReferenceStore(),
			spi.Witnessing:    newReferenceStore(),
			spi.Like:          newReferenceStore(),
			spi.Liked:         newReferenceStore(),
			spi.Share:         newReferenceStore(),
			spi.AnchorLinkset: newReferenceStore(),
		},
	}
}

// AddActivity adds the given activity to the activity store.
func (s *Store) AddActivity(activity *vocab.ActivityType) error {
	s.logger.Debug("Storing activity", log.WithActivityType(activity.Type().String()), log.WithActivityID(activity.ID()))

	return s.activityStore.add(activity)
}

// GetActivity returns the activity for the given ID from the activity store
// or ErrNotFound error if it wasn't found.
func (s *Store) GetActivity(activityID *url.URL) (*vocab.ActivityType, error) {
	s.logger.Debug("Retrieving activity", log.WithActivityID(activityID))

	return s.activityStore.get(activityID.String())
}

// QueryActivities queries the given activity store using the provided criteria
// and returns a results iterator.
func (s *Store) QueryActivities(query *spi.Criteria, opts ...spi.QueryOpt) (spi.ActivityIterator, error) {
	s.logger.Debug("Querying activities", log.WithQuery(query))

	if query.ReferenceType != "" && query.ObjectIRI != nil {
		return s.queryActivitiesByRef(query.ReferenceType, query, opts...)
	}

	return s.activityStore.query(query, opts...), nil
}

// AddReference adds the reference of the given type to the given object.
func (s *Store) AddReference(referenceType spi.ReferenceType, objectIRI *url.URL, referenceIRI *url.URL,
	refMetaDataOpts ...spi.RefMetadataOpt) error {
	s.logger.Debug("Adding reference to object", log.WithReferenceType(string(referenceType)),
		log.WithObjectIRI(objectIRI), log.WithReferenceIRI(referenceIRI))

	if objectIRI == nil {
		return fmt.Errorf("nil object IRI")
	}

	if referenceIRI == nil {
		return fmt.Errorf("nil reference IRI")
	}

	return s.referenceStores[referenceType].add(objectIRI, referenceIRI)
}

// DeleteReference deletes the reference of the given type from the given actor.
func (s *Store) DeleteReference(referenceType spi.ReferenceType, objectIRI, referenceIRI *url.URL) error {
	s.logger.Debug("Deleting reference", log.WithReferenceType(string(referenceType)),
		log.WithObjectIRI(objectIRI), log.WithReferenceIRI(referenceIRI))

	if objectIRI == nil {
		return fmt.Errorf("nil object IRI")
	}

	if referenceIRI == nil {
		return fmt.Errorf("nil reference IRI")
	}

	return s.referenceStores[referenceType].delete(objectIRI, referenceIRI)
}

// QueryReferences returns the list of references of the given type according to the given query.
func (s *Store) QueryReferences(refType spi.ReferenceType,
	query *spi.Criteria, opts ...spi.QueryOpt) (spi.ReferenceIterator, error) {
	s.logger.Debug("Querying references", log.WithReferenceType(string(refType)), log.WithQuery(query))

	return s.referenceStores[refType].query(query, opts...)
}

func (s *Store) queryActivitiesByRef(refType spi.ReferenceType, query *spi.Criteria,
	opts ...spi.QueryOpt) (spi.ActivityIterator, error) {
	it, err := s.QueryReferences(refType, query, opts...)
	if err != nil {
		return nil, err
	}

	options := storeutil.GetQueryOptions(opts...)

	refs, err := storeutil.ReadReferences(it, options.PageSize)
	if err != nil {
		return nil, err
	}

	totalItems, err := it.TotalItems()
	if err != nil {
		return nil, fmt.Errorf("failed to get total items from reference iterator: %w", err)
	}

	if len(refs) == 0 {
		return NewActivityIterator(nil, totalItems), nil
	}

	ait := s.activityStore.query(
		spi.NewCriteria(spi.WithActivityIRIs(refs...)),
		spi.WithSortOrder(options.SortOrder))

	// Set 'totalItems' to the 'totalItems' returned in the original reference query, which may be based on paging.
	ait.totalItems = totalItems

	return ait, nil
}

type activityStore struct {
	mutex        sync.RWMutex
	activities   []*vocab.ActivityType
	activityByID map[string]*vocab.ActivityType
}

func newActivitiesStore() *activityStore {
	return &activityStore{
		activityByID: make(map[string]*vocab.ActivityType),
	}
}

func (s *activityStore) add(activity *vocab.ActivityType) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.activities = append(s.activities, activity)
	s.activityByID[activity.ID().String()] = activity

	return nil
}

func (s *activityStore) get(activityID string) (*vocab.ActivityType, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	a, ok := s.activityByID[activityID]
	if !ok {
		return nil, spi.ErrNotFound
	}

	return a, nil
}

func (s *activityStore) query(query *spi.Criteria, opts ...spi.QueryOpt) *ActivityIterator {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	return NewActivityIterator(activityQueryResults(s.activities).filter(query, opts...))
}

type referenceStore struct {
	irisByObject map[string][]*url.URL
	mutex        sync.RWMutex
}

func newReferenceStore() *referenceStore {
	return &referenceStore{
		irisByObject: make(map[string][]*url.URL),
	}
}

func (s *referenceStore) add(actor fmt.Stringer, iri *url.URL) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	actorID := actor.String()

	s.irisByObject[actorID] = append(s.irisByObject[actorID], iri)

	return nil
}

func (s *referenceStore) delete(actor, iri fmt.Stringer) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	irisForActor := s.irisByObject[actor.String()]

	for actorIRI, i := range irisForActor {
		if i.String() == iri.String() {
			s.irisByObject[actor.String()] = append(irisForActor[0:actorIRI], irisForActor[actorIRI+1:]...)

			return nil
		}
	}

	return nil
}

func (s *referenceStore) query(query *spi.Criteria, opts ...spi.QueryOpt) (spi.ReferenceIterator, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if query.ObjectIRI == nil {
		return nil, fmt.Errorf("object IRI is required")
	}

	return NewReferenceIterator(refQueryResults(s.irisByObject[query.ObjectIRI.String()]).filter(query, opts...)), nil
}

type activityQueryFilter struct {
	*spi.Criteria
}

func newQueryFilter(query *spi.Criteria) *activityQueryFilter {
	return &activityQueryFilter{
		Criteria: query,
	}
}

func (q *activityQueryFilter) apply(activities []*vocab.ActivityType) []*vocab.ActivityType {
	var results []*vocab.ActivityType

	if len(q.ActivityIRIs) > 0 {
		for _, a := range activities {
			if containsIRI(q.ActivityIRIs, a.ID().URL()) {
				results = append(results, a)
			}
		}

		return results
	}

	for _, a := range activities {
		if len(q.Types) == 0 || a.Type().IsAny(q.Types...) {
			results = append(results, a)
		}
	}

	return results
}

type activityQueryResults []*vocab.ActivityType

func (r activityQueryResults) filter(query *spi.Criteria, opts ...spi.QueryOpt) ([]*vocab.ActivityType, int) {
	results := newQueryFilter(query).apply(r)

	options := storeutil.GetQueryOptions(opts...)

	if options.SortOrder == spi.SortDescending {
		reverseSort(results)
	}

	startIdx := getStartIndex(len(results), options)
	if startIdx == -1 {
		return nil, len(results)
	}

	return results[startIdx:], len(results)
}

type refQueryResults []*url.URL

func (r refQueryResults) filter(query *spi.Criteria, opts ...spi.QueryOpt) ([]*url.URL, int) {
	results := newRefQueryFilter(query).apply(r)

	options := storeutil.GetQueryOptions(opts...)

	if options.SortOrder == spi.SortDescending {
		reverseSort(results)
	}

	startIdx := getStartIndex(len(results), options)
	if startIdx == -1 {
		return nil, len(results)
	}

	return results[startIdx:], len(results)
}

type refQueryFilter struct {
	*spi.Criteria
}

func newRefQueryFilter(query *spi.Criteria) *refQueryFilter {
	return &refQueryFilter{
		Criteria: query,
	}
}

func (f *refQueryFilter) apply(refs []*url.URL) []*url.URL {
	var results []*url.URL

	for _, ref := range refs {
		if f.ReferenceIRI == nil || ref.String() == f.ReferenceIRI.String() {
			results = append(results, ref)
		}
	}

	return results
}

func getFirstPageNum(totalItems, pageSize int) int {
	if totalItems%pageSize > 0 {
		return totalItems / pageSize
	}

	return totalItems/pageSize - 1
}

func getStartIndex(totalItems int, options *spi.QueryOptions) int {
	if options.PageSize <= 0 {
		return 0
	}

	startIdx := startIndex(totalItems, options)
	if startIdx < 0 || startIdx >= totalItems {
		return -1
	}

	return startIdx
}

func startIndex(totalItems int, options *spi.QueryOptions) int {
	if options.PageNumber < 0 {
		return 0
	}

	if options.SortOrder == spi.SortAscending {
		return options.PageNumber * options.PageSize
	}

	return (getFirstPageNum(totalItems, options.PageSize) - options.PageNumber) * options.PageSize
}

func reverseSort(results interface{}) {
	sort.SliceStable(results, func(i, j int) bool { return i > j }) //nolint:gocritic
}

func containsIRI(iris []*url.URL, id fmt.Stringer) bool {
	for _, iri := range iris {
		if iri.String() == id.String() {
			return true
		}
	}

	return false
}
