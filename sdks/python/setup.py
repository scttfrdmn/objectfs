#!/usr/bin/env python3
"""
ObjectFS Python SDK Setup
"""

from setuptools import setup, find_packages
import os

def read_long_description():
    """Read README for long description."""
    readme_path = os.path.join(os.path.dirname(__file__), 'README.md')
    if os.path.exists(readme_path):
        with open(readme_path, 'r', encoding='utf-8') as f:
            return f.read()
    return 'ObjectFS Python SDK - High-performance POSIX filesystem for object storage'

setup(
    name='objectfs',
    version='0.1.0',
    description='ObjectFS Python SDK - High-performance POSIX filesystem for object storage',
    long_description=read_long_description(),
    long_description_content_type='text/markdown',
    author='Scott Friedman',
    url='https://github.com/scttfrdmn/objectfs',
    license='Apache-2.0',
    packages=find_packages(),
    classifiers=[
        'Development Status :: 4 - Beta',
        'Intended Audience :: Developers',
        # Apache 2.0, matching LICENSE and the Java SDK's pom. This said MIT, which is a different
        # licence than the one the code is under — and package metadata is what downstream tooling
        # reads, so a licence scanner would have reported MIT for an Apache 2.0 project.
        'License :: OSI Approved :: Apache Software License',
        'Programming Language :: Python :: 3',
        'Programming Language :: Python :: 3.8',
        'Programming Language :: Python :: 3.9',
        'Programming Language :: Python :: 3.10',
        'Programming Language :: Python :: 3.11',
        'Programming Language :: Python :: 3.12',
        'Topic :: System :: Filesystems',
        'Topic :: Software Development :: Libraries :: Python Modules',
    ],
    python_requires='>=3.8',
    # Ranges, deliberately, and not pins: a library that pins its transitive tree pins it for every
    # consumer. The pinned tree lives in requirements.txt, which is what CI installs and what
    # `trivy fs` scans — see the header there for why that split is the standard one, and why the
    # filename cannot be changed.
    #
    # Two entries were removed rather than bumped, because neither was a dependency of this package:
    #
    #   asyncio — a *stdlib module* since Python 3.4. The PyPI distribution of that name is a
    #   backport for 3.3, and `python_requires` here is >=3.8. Verified in a scratch venv: with the
    #   package installed, `asyncio.__file__` still resolves to the stdlib, and asyncio 4.0.0 is now
    #   a deliberate empty stub ("Deprecated backport of asyncio; use the stdlib package instead")
    #   that ships dist-info and no modules. So it shadowed nothing — but it declared a dependency
    #   the SDK does not have, and older resolutions of that range are not empty stubs.
    #
    #   typing-extensions — imported nowhere in this package. `grep -rn typing_extensions
    #   sdks/python/` finds no hit outside egg-info. It still appears in requirements.txt because
    #   pytest pulls it in transitively, which is the difference between "what CI installs" and
    #   "what this package requires."
    #
    # The four that remain are each imported by name: requests, yaml (pyyaml), psutil, aiohttp. The
    # suite's 71 tests pass with the two removals applied.
    install_requires=[
        'requests>=2.25.0',
        'pyyaml>=6.0',
        'psutil>=5.8.0',
        'aiohttp>=3.8.0',
    ],
    extras_require={
        'dev': [
            'pytest>=6.0',
            'pytest-asyncio>=0.18.0',
            'pytest-cov>=3.0.0',
            'black>=22.0.0',
            'isort>=5.10.0',
            'mypy>=0.950',
            'flake8>=4.0.0',
        ],
        'monitoring': [
            'prometheus-client>=0.14.0',
            'opentelemetry-api>=1.12.0',
            'opentelemetry-sdk>=1.12.0',
        ],
    },
    entry_points={
        'console_scripts': [
            'objectfs-python=objectfs.cli:main',
        ],
    },
    project_urls={
        'Bug Reports': 'https://github.com/scttfrdmn/objectfs/issues',
        'Source': 'https://github.com/scttfrdmn/objectfs',
        # This said `https://docs.objectfs.io/python`, which has never served anything and now cannot:
        # Porkbun answers a wildcard for objectfs.io, so the subdomain resolves like every other name
        # under the domain and completes no TLS handshake. The documentation is published at
        # objectfs.io/docs/, and there is no /python page in that tree, so this points at the SDK's own
        # README — the file `long_description` above is already built from, and the only Python
        # documentation that exists.
        #
        # `docs.` is not a name to reinstate later without checking. Five subdomains of objectfs.io
        # appear across this repository's history and exactly one, the apex, has ever answered;
        # `get.objectfs.io` was the first command in the getting-started guide for a year on the
        # strength of resolving.
        'Documentation': 'https://github.com/scttfrdmn/objectfs/blob/main/sdks/python/README.md',
    },
    keywords='filesystem, object-storage, s3, fuse, distributed, cache, performance',
    zip_safe=False,
    include_package_data=True,
    package_data={
        'objectfs': ['py.typed'],
    },
)
