import React, { useMemo } from 'react';
import Box from '@material-ui/core/Box';
import {
  useHistory,
  useParams,
} from 'react-router-dom';
import PageTitle from '../components/Layout/PageTitle';
import Table from '../components/Table/Table';
import PageContent from '../components/Layout/PageContent';

const ApplicationDeployments = () => {
  const { appName } = useParams();
  const history = useHistory();
  const onRowClick = (row) => () => {
    // redirect to application deployment page
    history.push({
      pathname: `/application/${row.id}`,
    });
  };
  const filters = useMemo(() => ({
    distinct: true,
    name: appName,
  }), []);
  return (
    <PageContent>
      <Box m={3}>
        <PageTitle>
                  Application:
          {' '}
          {appName}
        </PageTitle>
      </Box>
      <Box>
        <Table hideNameFilter={true} filters={filters} onRowClick={onRowClick} />
      </Box>
    </PageContent>
  );
};

export default ApplicationDeployments;