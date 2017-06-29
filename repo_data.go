package main

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/bradfitz/slice"
	"sync"
)

type OnCommitsLoaded func(*Oid) error
type OnBranchesLoaded func([]*Branch) error
type OnTagsLoaded func([]*Tag) error

type RepoData interface {
	Path() string
	LoadHead() error
	LoadLocalBranches(OnBranchesLoaded) error
	LoadLocalTags(OnTagsLoaded) error
	LoadCommits(*Oid, OnCommitsLoaded) error
	Head() (*Oid, *Branch)
	LocalBranches() (branches []*Branch, loading bool)
	LocalTags() (tags []*Tag, loading bool)
	RefsForCommit(*Commit) *CommitRefs
	CommitSetState(*Oid) CommitSetState
	Commits(oid *Oid, startIndex, count uint) (<-chan *Commit, error)
	CommitByIndex(oid *Oid, index uint) (*Commit, error)
	Commit(oid *Oid) (*Commit, error)
	Diff(commit *Commit) (*Diff, error)
}

type CommitSet struct {
	commits []*Commit
	loading bool
	lock    sync.Mutex
}

type CommitSetState struct {
	loading   bool
	commitNum uint
}

type BranchSet struct {
	branches     map[*Oid]*Branch
	branchesList []*Branch
	loading      bool
	lock         sync.Mutex
}

type TagSet struct {
	tags     map[*Oid]*Tag
	tagsList []*Tag
	loading  bool
	lock     sync.Mutex
}

type CommitRefs struct {
	tags     []*Tag
	branches []*Branch
}

type CommitRefSet struct {
	commitRefs map[*Oid]*CommitRefs
	lock       sync.Mutex
}

type RepositoryData struct {
	channels       *Channels
	repoDataLoader *RepoDataLoader
	head           *Oid
	headBranch     *Branch
	localBranches  *BranchSet
	localTags      *TagSet
	commitRefSet   *CommitRefSet
	commits        map[*Oid]*CommitSet
}

func NewBranchSet() *BranchSet {
	return &BranchSet{
		branches: make(map[*Oid]*Branch),
	}
}

func NewTagSet() *TagSet {
	return &TagSet{
		tags: make(map[*Oid]*Tag),
	}
}

func NewCommitRefSet() *CommitRefSet {
	return &CommitRefSet{
		commitRefs: make(map[*Oid]*CommitRefs),
	}
}

func (commitRefSet *CommitRefSet) AddTagForCommit(commit *Commit, tag *Tag) {
	commitRefSet.lock.Lock()
	defer commitRefSet.lock.Unlock()

	commitRefs, ok := commitRefSet.commitRefs[commit.oid]
	if !ok {
		commitRefs = &CommitRefs{}
		commitRefSet.commitRefs[commit.oid] = commitRefs
	}

	for _, tag := range commitRefs.tags {
		if tag.oid.oid.Equal(tag.oid.oid) {
			return
		}
	}

	commitRefs.tags = append(commitRefs.tags, tag)
}

func (commitRefSet *CommitRefSet) RefsForCommit(commit *Commit) (commitRefsCopy *CommitRefs) {
	commitRefSet.lock.Lock()
	defer commitRefSet.lock.Unlock()

	commitRefsCopy = &CommitRefs{}

	commitRefs, ok := commitRefSet.commitRefs[commit.oid]
	if ok {
		commitRefsCopy.tags = append([]*Tag(nil), commitRefs.tags...)
		commitRefsCopy.branches = append([]*Branch(nil), commitRefs.branches...)
	}

	return commitRefsCopy
}

func NewRepositoryData(repoDataLoader *RepoDataLoader, channels *Channels) *RepositoryData {
	return &RepositoryData{
		channels:       channels,
		repoDataLoader: repoDataLoader,
		localBranches:  NewBranchSet(),
		localTags:      NewTagSet(),
		commitRefSet:   NewCommitRefSet(),
		commits:        make(map[*Oid]*CommitSet),
	}
}

func (repoData *RepositoryData) Free() {
	repoData.repoDataLoader.Free()
}

func (repoData *RepositoryData) Initialise(repoPath string) (err error) {
	if err = repoData.repoDataLoader.Initialise(repoPath); err != nil {
		return
	}

	return
}

func (repoData *RepositoryData) Path() string {
	return repoData.repoDataLoader.Path()
}

func (repoData *RepositoryData) LoadHead() (err error) {
	head, branch, err := repoData.repoDataLoader.Head()
	if err != nil {
		return
	}

	repoData.head = head
	repoData.headBranch = branch

	return
}

func (repoData *RepositoryData) LoadLocalBranches(onBranchesLoaded OnBranchesLoaded) (err error) {
	branchSet := repoData.localBranches
	branchSet.lock.Lock()
	defer branchSet.lock.Unlock()

	if branchSet.loading {
		log.Debug("Local branches already loading")
		return
	}

	go func() {
		branches, err := repoData.repoDataLoader.LocalBranches()
		if err != nil {
			repoData.channels.ReportError(err)
			return
		}

		slice.Sort(branches, func(i, j int) bool {
			return branches[i].name < branches[j].name
		})

		branchMap := make(map[*Oid]*Branch)
		for _, branch := range branches {
			branchMap[branch.oid] = branch
		}

		branchSet.lock.Lock()
		branchSet.branches = branchMap
		branchSet.branchesList = branches
		branchSet.loading = false
		branchSet.lock.Unlock()

		repoData.channels.ReportError(onBranchesLoaded(branches))
	}()

	branchSet.loading = true

	return
}

func (repoData *RepositoryData) LoadLocalTags(onTagsLoaded OnTagsLoaded) (err error) {
	tagSet := repoData.localTags
	tagSet.lock.Lock()
	defer tagSet.lock.Unlock()

	if tagSet.loading {
		log.Debug("Local tags already loading")
		return
	}

	go func() {
		tags, err := repoData.repoDataLoader.LocalTags()
		if err != nil {
			repoData.channels.ReportError(err)
			return
		}

		slice.Sort(tags, func(i, j int) bool {
			return tags[i].name < tags[j].name
		})

		tagMap := make(map[*Oid]*Tag)
		for _, tag := range tags {
			tagMap[tag.oid] = tag
		}

		tagSet.lock.Lock()
		tagSet.tags = tagMap
		tagSet.tagsList = tags
		tagSet.loading = false
		tagSet.lock.Unlock()

		repoData.channels.ReportError(repoData.mapTagsToCommits())
		repoData.channels.ReportError(onTagsLoaded(tags))
	}()

	tagSet.loading = true

	return
}

func (repoData *RepositoryData) mapTagsToCommits() (err error) {
	tagSet := repoData.localTags
	tagSet.lock.Lock()
	defer tagSet.lock.Unlock()

	commitRefSet := repoData.commitRefSet

	for _, tag := range tagSet.tagsList {
		var commit *Commit
		commit, err = repoData.repoDataLoader.Commit(tag.oid)
		if err != nil {
			return
		}

		commitRefSet.AddTagForCommit(commit, tag)
	}

	return
}

func (repoData *RepositoryData) LoadCommits(oid *Oid, onCommitsLoaded OnCommitsLoaded) (err error) {
	if _, ok := repoData.commits[oid]; ok {
		log.Debugf("Commits already loading/loaded for oid %v", oid)
		return
	}

	commitCh, err := repoData.repoDataLoader.Commits(oid)
	if err != nil {
		return
	}

	commitSet := &CommitSet{
		loading: true,
		commits: make([]*Commit, 0),
	}
	repoData.commits[oid] = commitSet

	go func() {
		log.Debugf("Receiving commits from RepoDataLoader for oid %v", oid)

		for commit := range commitCh {
			commitSet.lock.Lock()
			commitSet.commits = append(commitSet.commits, commit)
			commitSet.lock.Unlock()
		}

		commitSet.lock.Lock()
		commitSet.loading = false
		commitSet.lock.Unlock()
		log.Debugf("Finished loading commits for oid %v", oid)

		repoData.channels.ReportError(onCommitsLoaded(oid))
	}()

	return
}

func (repoData *RepositoryData) Head() (*Oid, *Branch) {
	return repoData.head, repoData.headBranch
}

func (repoData *RepositoryData) LocalBranches() (branches []*Branch, loading bool) {
	branchSet := repoData.localBranches
	branchSet.lock.Lock()
	defer branchSet.lock.Unlock()

	branches = branchSet.branchesList
	loading = branchSet.loading

	return
}

func (repoData *RepositoryData) LocalTags() (tags []*Tag, loading bool) {
	tagSet := repoData.localTags
	tagSet.lock.Lock()
	defer tagSet.lock.Unlock()

	tags = tagSet.tagsList
	loading = tagSet.loading

	return
}

func (repoData *RepositoryData) RefsForCommit(commit *Commit) *CommitRefs {
	return repoData.commitRefSet.RefsForCommit(commit)
}

func (repoData *RepositoryData) CommitSetState(oid *Oid) CommitSetState {
	var commitSetState CommitSetState

	if commitSet, ok := repoData.commits[oid]; !ok {
		commitSetState = CommitSetState{
			loading:   false,
			commitNum: 0,
		}
	} else {
		commitSet.lock.Lock()
		commitSetState = CommitSetState{
			loading:   commitSet.loading,
			commitNum: uint(len(commitSet.commits)),
		}
		commitSet.lock.Unlock()
	}

	return commitSetState
}

func (repoData *RepositoryData) Commits(oid *Oid, startIndex, count uint) (<-chan *Commit, error) {
	var commitSet *CommitSet
	var ok bool

	if commitSet, ok = repoData.commits[oid]; !ok {
		return nil, fmt.Errorf("No commits loaded for oid %v", oid)
	}

	commitCh := make(chan *Commit)

	go func() {
		defer close(commitCh)
		var commit *Commit
		index := startIndex

		for {
			commit = nil
			commitSet.lock.Lock()

			if index < uint(len(commitSet.commits)) && index-startIndex < count {
				commit = commitSet.commits[index]
				index++
			}

			commitSet.lock.Unlock()

			if commit != nil {
				commitCh <- commit
			} else {
				return
			}
		}
	}()

	return commitCh, nil
}

func (repoData *RepositoryData) CommitByIndex(oid *Oid, index uint) (commit *Commit, err error) {
	commitSet, ok := repoData.commits[oid]
	if !ok {
		return nil, fmt.Errorf("No commits loaded for oid %v", oid)
	}

	commitSet.lock.Lock()
	defer commitSet.lock.Unlock()

	commitNum := uint(len(commitSet.commits))

	if index < commitNum {
		commit = commitSet.commits[index]
	} else {
		err = fmt.Errorf("Commit index %v is invalid for branch %v with %v commits", index, oid, commitNum)
	}

	return
}

func (repoData *RepositoryData) Commit(oid *Oid) (*Commit, error) {
	return repoData.repoDataLoader.Commit(oid)
}

func (repoData *RepositoryData) Diff(commit *Commit) (*Diff, error) {
	return repoData.repoDataLoader.Diff(commit)
}
