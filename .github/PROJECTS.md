# ObjectFS GitHub Projects

This document describes the GitHub Projects setup for ObjectFS, following the Prism organizational model.

## Project Boards

### 1. ObjectFS v0.5.0 Development
**URL**: https://github.com/users/scttfrdmn/projects/11

Main development board tracking all v0.5.0 work (features + technical debt).

**Custom Fields**:
- **Priority**: Critical, High, Medium, Low
- **Effort**: XS (< 2h), Small (2-4h), Medium (1-2d), Large (3-5d), XL (1-2w)
- **Phase**: Phase 1-4, Technical Debt
- **Persona**: Primary beneficiary persona
- **Status**: Todo, In Progress, Done (default)

**Contains**: 13 issues (5 technical debt + 8 features)

### 2. ObjectFS Technical Debt
**URL**: https://github.com/users/scttfrdmn/projects/12

Dedicated board for tracking technical debt and code quality improvements.

**Custom Fields**:
- **Priority**: Critical, High, Medium, Low
- **Effort**: XS, Small, Medium, Large, XL
- **Category**: Test Coverage, Code Refactoring, Performance, Security, Configuration
- **Status**: Todo, In Progress, Done (default)

**Contains**: 5 issues (buffer testing, FUSE testing, multipart refactoring, region config, remediation TODOs)

## Using the Projects

### Adding Issues to Projects

```bash
# Add issue to v0.5.0 Development board
gh project item-add 11 --owner scttfrdmn --url https://github.com/scttfrdmn/objectfs/issues/NUMBER

# Add issue to Technical Debt board
gh project item-add 12 --owner scttfrdmn --url https://github.com/scttfrdmn/objectfs/issues/NUMBER
```

### Viewing Projects

```bash
# View v0.5.0 Development board
gh project view 11 --owner scttfrdmn --web

# View Technical Debt board
gh project view 12 --owner scttfrdmn --web
```

### Listing Fields

```bash
# List all custom fields in v0.5.0 board
gh project field-list 11 --owner scttfrdmn

# List all custom fields in Technical Debt board
gh project field-list 12 --owner scttfrdmn
```

## Workflow

### Issue Lifecycle

1. **New Issue** → Automatically appears in "Todo" column
2. **Assign to yourself** → Move to "In Progress"
3. **Create PR** → PR automatically links to issue
4. **PR merged** → Move to "Done"

### Prioritization Guidelines

**Priority Field**:
- **Critical**: Blocking release, security vulnerability, data corruption risk
- **High**: Important for milestone, significant user impact
- **Medium**: Should be done but not urgent
- **Low**: Nice to have, future consideration

**Effort Field**:
- **XS**: < 2 hours (quick fix, simple config change)
- **Small**: 2-4 hours (isolated feature, small refactor)
- **Medium**: 1-2 days (substantial feature, moderate refactor)
- **Large**: 3-5 days (complex feature, significant refactor)
- **XL**: 1-2 weeks (major feature, architectural change)

### Phase Tracking

Issues are tagged with their v0.5.0 phase:

- **Phase 1: CargoShip** (Weeks 1-12): Archive, lifecycle, BBR
- **Phase 2: Compression** (Weeks 13-19): ZSTD, LZ4, adaptive selection
- **Phase 3: Distributed** (Weeks 20-26): Redis cache, multi-node (experimental)
- **Phase 4: Cost Optimization** (Weeks 27-31): ML prediction, cost tracking
- **Technical Debt**: Integrated throughout (Option B approach)

## Views

### Recommended Views to Create

1. **By Phase** - Group by Phase field to see work distribution
2. **By Priority** - Sort by Priority to see critical items first
3. **By Persona** - Group by Persona to ensure all users are served
4. **Timeline** - Roadmap view using milestones
5. **Blocked Items** - Filter for issues with "status: blocked" label

### Creating Views in UI

1. Go to project board
2. Click "+ New view"
3. Choose view type (Board, Table, Roadmap)
4. Configure filters, grouping, sorting

## Integration with Labels and Milestones

Projects work alongside:
- **Labels**: Multi-dimensional categorization (90 labels)
- **Milestones**: Time-based organization (5 milestones)
- **Projects**: Board-based workflow management

All three together provide comprehensive project management:
- **Filter by label** in UI to find specific types of work
- **Filter by milestone** to see phase-specific work
- **Use project board** for day-to-day workflow (Todo → In Progress → Done)

## Automation Opportunities

GitHub Projects v2 supports automated workflows:

1. **Auto-add issues** with specific labels
2. **Auto-archive** when closed
3. **Auto-assign** based on code owners
4. **Status sync** with PR state

These can be configured in the project settings UI.

## Quick Links

- [v0.5.0 Development Board](https://github.com/users/scttfrdmn/projects/11)
- [Technical Debt Board](https://github.com/users/scttfrdmn/projects/12)
- [All Issues](https://github.com/scttfrdmn/objectfs/issues)
- [All Milestones](https://github.com/scttfrdmn/objectfs/milestones)
- [All Labels](https://github.com/scttfrdmn/objectfs/labels)
