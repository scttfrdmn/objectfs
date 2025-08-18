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
    author='ObjectFS Team',
    author_email='team@objectfs.io',
    url='https://github.com/objectfs/objectfs',
    packages=find_packages(),
    classifiers=[
        'Development Status :: 4 - Beta',
        'Intended Audience :: Developers',
        'License :: OSI Approved :: MIT License',
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
    install_requires=[
        'requests>=2.25.0',
        'pyyaml>=6.0',
        'psutil>=5.8.0',
        'aiohttp>=3.8.0',
        'asyncio>=3.4.3',
        'typing-extensions>=4.0.0',
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
        'Bug Reports': 'https://github.com/objectfs/objectfs/issues',
        'Source': 'https://github.com/objectfs/objectfs',
        'Documentation': 'https://docs.objectfs.io/python',
    },
    keywords='filesystem, object-storage, s3, fuse, distributed, cache, performance',
    zip_safe=False,
    include_package_data=True,
    package_data={
        'objectfs': ['py.typed'],
    },
)
